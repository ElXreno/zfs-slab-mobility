package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"testing"
)

func realisticImage(t testing.TB) *pixmap {
	const blocks = 31000
	m := &blockMap{nblocks: blocks, counts: make([][nrClasses]uint16, blocks)}
	for i := range m.counts {
		m.counts[i][clFree] = uint16(512 - i%400)
		m.counts[i][clSlab] = uint16(i % 400 / 2)
		m.counts[i][clABD] = uint16(i % 400 / 2)
	}
	f := &frame{m: m, pagesPerBlock: 512, viewTo: blocks}
	f.summarise()
	f.paintPixels(209, 27, 9, 19)
	return &f.px
}

func BenchmarkEncodeZlib(b *testing.B) {
	p := realisticImage(b)
	b.SetBytes(int64(len(p.img.Pix)))
	for b.Loop() {
		var buf bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		zw.Write(p.img.Pix)
		zw.Close()
	}
}

func BenchmarkEncodeBase64(b *testing.B) {
	p := realisticImage(b)
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	zw.Write(p.img.Pix)
	zw.Close()
	b.Logf("%d bytes RGBA -> %d after zlib -> %d in base64",
		len(p.img.Pix), buf.Len(), base64.StdEncoding.EncodedLen(buf.Len()))
	for b.Loop() {
		base64.StdEncoding.EncodeToString(buf.Bytes())
	}
}
