package hfs

func parseExtentsRecord(payload []byte) ([]ExtentDescriptor, error) {
	const extRecSize = 8 * 8
	if len(payload) < extRecSize {
		return nil, &ParseError{Op: "parse_extents_record", Offset: 0, Err: ErrCorrupt}
	}

	extents := make([]ExtentDescriptor, 0, 8)
	for i := 0; i < 8; i++ {
		base := i * 8
		e := ExtentDescriptor{
			StartBlock: be32(payload[base : base+4]),
			BlockCount: be32(payload[base+4 : base+8]),
		}
		if e.StartBlock == 0 && e.BlockCount == 0 {
			break
		}
		extents = append(extents, e)
	}
	return extents, nil
}

func compactExtents(input []ExtentDescriptor) []ExtentDescriptor {
	out := make([]ExtentDescriptor, 0, len(input))
	for _, e := range input {
		if e.StartBlock == 0 && e.BlockCount == 0 {
			break
		}
		if e.BlockCount == 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

func extentBlockCount(exts []ExtentDescriptor) uint32 {
	var total uint32
	for _, e := range exts {
		total += e.BlockCount
	}
	return total
}

func trimExtentsToBlocks(exts []ExtentDescriptor, blocks uint32) []ExtentDescriptor {
	if blocks == 0 {
		return nil
	}
	out := make([]ExtentDescriptor, 0, len(exts))
	var seen uint32
	for _, e := range exts {
		if seen >= blocks {
			break
		}
		need := blocks - seen
		if e.BlockCount <= need {
			out = append(out, e)
			seen += e.BlockCount
			continue
		}
		out = append(out, ExtentDescriptor{StartBlock: e.StartBlock, BlockCount: need})
		seen += need
		break
	}
	return out
}

func (v *Volume) ResolveDataForkExtents(cnid uint32) ([]ExtentDescriptor, error) {
	return v.resolveForkExtents(cnid, false)
}

func (v *Volume) ResolveResourceForkExtents(cnid uint32) ([]ExtentDescriptor, error) {
	return v.resolveForkExtents(cnid, true)
}

func (v *Volume) resolveForkExtents(cnid uint32, resource bool) ([]ExtentDescriptor, error) {
	rec, err := v.OpenCNID(cnid)
	if err != nil {
		return nil, err
	}

	fork := rec.DataFork
	forkType := extentKeyTypeData
	if resource {
		fork = rec.RsrcFork
		forkType = extentKeyTypeRsrc
	}
	return v.resolveForkExtentsFromFork(cnid, fork, forkType)
}

func (v *Volume) resolveForkExtentsFromFork(cnid uint32, fork ForkData, forkType uint8) ([]ExtentDescriptor, error) {
	out := compactExtents(fork.Extents[:])
	if fork.TotalBlocks == 0 {
		return out, nil
	}

	covered := extentBlockCount(out)
	if covered >= fork.TotalBlocks {
		return trimExtentsToBlocks(out, fork.TotalBlocks), nil
	}

	overflow := make(map[uint32][]ExtentDescriptor)
	err := v.walkExtentsBTree(func(key ExtentsKey, payload []byte) error {
		if key.FileID != cnid || key.ForkType != forkType {
			return nil
		}
		extents, err := parseExtentsRecord(payload)
		if err != nil {
			return nil
		}
		overflow[key.StartBlock] = compactExtents(extents)
		return nil
	})
	if err != nil {
		return nil, err
	}

	for covered < fork.TotalBlocks {
		next, ok := overflow[covered]
		if !ok || len(next) == 0 {
			return nil, &ParseError{Op: "resolve_fork_extents", Offset: int64(cnid), Err: ErrMissingExtent}
		}
		before := covered
		out = append(out, next...)
		covered = extentBlockCount(out)
		if covered <= before {
			return nil, &ParseError{Op: "resolve_fork_extents", Offset: int64(cnid), Err: ErrCorrupt}
		}
	}

	out = trimExtentsToBlocks(out, fork.TotalBlocks)
	return out, nil
}
