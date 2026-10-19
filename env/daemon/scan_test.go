package daemon

import "testing"

// Upstream's scan.rs test: lines located and measured as decoding the whole
// file would, however the file is chunked.

func scanChunks(file []byte, start, end uint64, hasEnd bool, chunk int) object {
	s := newLineScanner(start, end, hasEnd)
	for i := 0; i < len(file); i += chunk {
		s.push(file[i:min(i+chunk, len(file))])
	}
	return s.finish()
}

func TestLineScannerLocatesAndMeasuresLinesLikeWholeFileDecoding(t *testing.T) {
	// BOM, "a", invalid sequence, empty line, a later U+FEFF, and no final
	// newline.
	file := []byte{0xef, 0xbb, 0xbf, 0x61, 0x0a, 0xe2, 0x82, 0x0a, 0x0a, 0xef, 0xbb, 0xbf, 0x62, 0x0a, 0xc3, 0xa9}
	for chunk := 1; chunk < 6; chunk++ {
		all := scanChunks(file, 0, 0, false, chunk)
		// "a\n\ufffd\n\n\ufeffb\né" is 1+1+3+1+1+3+1+1+2 = 14 bytes.
		if all["newlines"] != uint64(4) || all["selectedBytes"] != uint64(14) || all["firstLineBytes"] != uint64(1) {
			t.Fatalf("chunk %d: whole file %v", chunk, all)
		}
		middle := scanChunks(file, 1, 3, true, chunk)
		if middle["start"] != uint64(5) || middle["end"] != uint64(8) || middle["selectedBytes"] != uint64(4) || middle["lastLineStart"] != uint64(8) {
			t.Fatalf("chunk %d: lines 1-3 %v", chunk, middle)
		}
		if past := scanChunks(file, 9, 0, false, chunk); past["start"] != uint64(16) {
			t.Fatalf("chunk %d: past the end %v", chunk, past)
		}
	}
}
