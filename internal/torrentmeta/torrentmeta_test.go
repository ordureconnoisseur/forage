package torrentmeta

import (
	"testing"
)

func TestParseSingleFile(t *testing.T) {
	// d4:infod6:lengthi1024e4:name9:movie.mp4ee
	raw := []byte("d4:infod6:lengthi1024e4:name9:movie.mp4ee")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "movie.mp4" || m.TotalSize != 1024 || m.FileCount != 1 || m.VideoCount != 1 {
		t.Fatalf("got %+v", m)
	}
}

func TestParseMultiFile(t *testing.T) {
	// info{ files:[ {2000,a.mp4}, {30,a.jpg} ], name:pack }
	raw := []byte("d4:infod5:filesld6:lengthi2000e4:pathl5:a.mp4eed6:lengthi30e4:pathl5:a.jpgeee4:name4:packee")
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Name != "pack" {
		t.Errorf("name = %q", m.Name)
	}
	if m.TotalSize != 2030 {
		t.Errorf("size = %d", m.TotalSize)
	}
	if m.FileCount != 2 {
		t.Errorf("filecount = %d", m.FileCount)
	}
	if m.VideoCount != 1 {
		t.Errorf("videocount = %d (want 1, .jpg must not count)", m.VideoCount)
	}
}
