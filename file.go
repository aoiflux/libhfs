package hfs

import (
	"io"
)

type File struct {
	vol      *Volume
	rec      CatalogRecord
	extents  []ExtentDescriptor
	inline   []byte
	size     int64
	off      int64
	resource bool
}

func (f *File) CNID() uint32 { return f.rec.CNID }
func (f *File) Name() string { return f.rec.Name }
func (f *File) Size() int64  { return f.size }

func (f *File) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &ParseError{Op: "file_read_at", Offset: off, Err: ErrInvalidOffset}
	}
	if len(p) == 0 {
		return 0, nil
	}
	if f.inline != nil {
		if off >= int64(len(f.inline)) {
			return 0, io.EOF
		}
		n := copy(p, f.inline[off:])
		if off+int64(n) >= int64(len(f.inline)) {
			return n, io.EOF
		}
		return n, nil
	}
	if off >= f.size {
		return 0, io.EOF
	}

	want := len(p)
	remainFile := f.size - off
	if int64(want) > remainFile {
		want = int(remainFile)
	}

	blockSize := int64(f.vol.header.BlockSize)
	wrote := 0
	logicalBase := int64(0)
	curOff := off

	for _, ext := range f.extents {
		extBytes := int64(ext.BlockCount) * blockSize
		if curOff >= logicalBase+extBytes {
			logicalBase += extBytes
			continue
		}

		inExt := curOff - logicalBase
		if inExt < 0 {
			inExt = 0
		}
		can := extBytes - inExt
		need := int64(want - wrote)
		if need <= 0 {
			break
		}
		if can > need {
			can = need
		}

		physOff := int64(ext.StartBlock)*blockSize + inExt
		chunk := p[wrote : wrote+int(can)]
		if err := readAtExact(f.vol.reader, physOff, chunk); err != nil {
			return wrote, err
		}

		wrote += len(chunk)
		curOff += int64(len(chunk))
		logicalBase += extBytes
		if wrote >= want {
			break
		}
	}

	if wrote < want {
		if wrote == 0 {
			return 0, io.EOF
		}
		return wrote, io.EOF
	}
	if int64(off)+int64(wrote) >= f.size {
		return wrote, io.EOF
	}
	return wrote, nil
}

func (f *File) ReadAll() ([]byte, error) {
	if f.size == 0 {
		return []byte{}, nil
	}
	if f.size < 0 {
		return nil, &ParseError{Op: "file_read_all", Offset: f.size, Err: ErrCorrupt}
	}
	buf := make([]byte, f.size)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (v *Volume) OpenFileByCNID(cnid uint32) (*File, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return nil, err
	}
	return v.openFileFromRecord(rec, false)
}

func (v *Volume) OpenResourceForkByCNID(cnid uint32) (*File, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return nil, err
	}
	return v.openFileFromRecord(rec, true)
}

func (v *Volume) OpenFileByPath(path string) (*File, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return v.openFileFromRecord(rec, false)
}

func (v *Volume) OpenResourceForkByPath(path string) (*File, error) {
	rec, err := v.OpenPath(path)
	if err != nil {
		return nil, err
	}
	return v.openFileFromRecord(rec, true)
}

func (v *Volume) openFileFromRecord(rec CatalogRecord, resource bool) (*File, error) {
	if rec.Type != CatalogRecordFile {
		return nil, ErrNotFile
	}
	if !resource {
		inline, _, ok, err := v.readDecmpfsInline(rec.CNID)
		if err != nil {
			return nil, err
		}
		if ok {
			return &File{vol: v, rec: rec, inline: inline, size: int64(len(inline))}, nil
		}
	}
	var exts []ExtentDescriptor
	var err error
	var size uint64
	if resource {
		exts, err = v.ResolveResourceForkExtents(rec.CNID)
		size = rec.RsrcFork.LogicalSize
	} else {
		exts, err = v.ResolveDataForkExtents(rec.CNID)
		size = rec.DataFork.LogicalSize
	}
	if err != nil {
		return nil, err
	}
	return &File{vol: v, rec: rec, extents: exts, size: int64(size), resource: resource}, nil
}
