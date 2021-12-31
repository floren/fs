package main

import (
	"fmt"

	"github.com/floren/fs/internal/pack"
)

const (
	HeaderMagic   = 0x3776ae89
	HeaderVersion = 1
	HeaderOffset  = 128 * 1024
	HeaderSize    = 512
)

type Header struct {
	version   uint16
	blockSize uint16
	super     uint32 /* super blocks */
	label     uint32 /* start of labels */
	data      uint32 /* end of labels - start of data blocks */
	end       uint32 /* end of data blocks */
}

func (h *Header) pack(p []byte) {
	memset(p[:HeaderSize], 0)
	pack.PutUint32(p, HeaderMagic)
	pack.PutUint16(p[4:], HeaderVersion)
	pack.PutUint16(p[6:], h.blockSize)
	pack.PutUint32(p[8:], h.super)
	pack.PutUint32(p[12:], h.label)
	pack.PutUint32(p[16:], h.data)
	pack.PutUint32(p[20:], h.end)
}

func unpackHeader(p []byte) (*Header, error) {
	h := new(Header)

	if pack.GetUint32(p) != HeaderMagic {
		return nil, fmt.Errorf("vac header bad magic")
	}

	h.version = pack.GetUint16(p[4:])
	if h.version != HeaderVersion {
		return nil, fmt.Errorf("vac header bad version")
	}
	h.blockSize = pack.GetUint16(p[6:])
	h.super = pack.GetUint32(p[8:])
	h.label = pack.GetUint32(p[12:])
	h.data = pack.GetUint32(p[16:])
	h.end = pack.GetUint32(p[20:])

	return h, nil
}
