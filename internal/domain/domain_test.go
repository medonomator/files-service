package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMatchesContent(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10}
	mp4 := append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...)
	mov := append([]byte{0, 0, 0, 0x14}, []byte("ftypqt  ")...)
	pdf := []byte("%PDF-1.7\n")
	heic := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic")...)
	avif := append([]byte{0, 0, 0, 0x1c}, []byte("ftypavif")...)
	docx := []byte{'P', 'K', 0x03, 0x04, 0x14}
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	wav := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")

	cases := []struct {
		name        string
		contentType string
		head        []byte
		want        bool
	}{
		{"jpeg", "image/jpeg", jpeg, true},
		{"jpeg declared as png", "image/png", jpeg, false},
		{"mp4", "video/mp4", mp4, true},
		{"mov declared as mp4 shares the container", "video/mp4", mov, true},
		{"mov", "VIDEO/QuickTime", mov, true},
		{"pdf", "application/pdf", pdf, true},
		{"heic", "image/heic", heic, true},
		{"avif", "image/avif", avif, true},
		{"mp4 is not heic", "image/heic", mp4, false},
		{"heic is not mp4", "video/mp4", heic, false},
		{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", docx, true},
		{"webp is not wav", "audio/wav", webp, false},
		{"wav", "audio/wav", wav, true},
		{"utf8 text", "text/csv", []byte("имя,телефон\n"), true},
		{"binary as text", "text/plain", []byte{'a', 0, 'b'}, false},
		{"pdf as text", "text/plain", pdf, false},
		{"text cut mid rune", "text/plain", []byte("при")[:5], true},
		{"html is not in catalog", "text/html", []byte("<script>"), false},
		{"empty", "image/jpeg", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesContent(tc.contentType, tc.head); got != tc.want {
				t.Fatalf("MatchesContent(%q) = %v, want %v", tc.contentType, got, tc.want)
			}
		})
	}
}

func TestDangerousTypesAreNotInCatalog(t *testing.T) {
	for _, contentType := range []string{"text/html", "image/svg+xml", "application/javascript", "application/x-msdownload"} {
		if _, ok := LookupContentType(contentType); ok {
			t.Fatalf("%s must not be accepted", contentType)
		}
	}
}

func TestClientAllows(t *testing.T) {
	client := Client{AllowedTypes: []string{"image/*", "application/pdf"}}

	cases := map[string]bool{
		"image/png":       true,
		"Image/JPEG":      true,
		"application/pdf": true,
		"video/mp4":       false,
		"imagex/png":      false,
	}

	for contentType, want := range cases {
		if got := client.Allows(contentType); got != want {
			t.Fatalf("Allows(%q) = %v, want %v", contentType, got, want)
		}
	}

	if !(Client{AllowedTypes: []string{"*"}}).Allows("video/mp4") {
		t.Fatal("wildcard must allow everything")
	}
}

func TestValidTypePattern(t *testing.T) {
	for pattern, want := range map[string]bool{
		"*": true, "image/*": true, "video/mp4": true, "font/*": false, "text/html": false, "video": false,
	} {
		if got := ValidTypePattern(pattern); got != want {
			t.Fatalf("ValidTypePattern(%q) = %v, want %v", pattern, got, want)
		}
	}
}

func TestValidFolder(t *testing.T) {
	for folder, want := range map[string]bool{
		"lessons":               true,
		"lessons/maid":          true,
		"a/b/c/d":               true,
		"a/b/c/d/e":             false,
		"../etc":                false,
		"Lessons":               false,
		"lessons/":              false,
		"/lessons":              false,
		"lessons//maid":         false,
		strings.Repeat("a", 64): false,
	} {
		if got := ValidFolder(folder); got != want {
			t.Fatalf("ValidFolder(%q) = %v, want %v", folder, got, want)
		}
	}
}

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"Урок 1.mp4":                  "Урок 1.mp4",
		`C:\Users\olga\video "1".mov`: "video 1.mov",
		"../../etc/passwd":            "passwd",
		"name\r\nX-Header: 1.pdf":     "nameX-Header: 1.pdf",
		"":                            "file",
		"  ":                          "file",
		"/":                           "file",
	}

	for input, want := range cases {
		if got := CleanFileName(input); got != want {
			t.Fatalf("CleanFileName(%q) = %q, want %q", input, got, want)
		}
	}

	if got := []rune(CleanFileName(strings.Repeat("я", 300))); len(got) != maxFileNameRunes {
		t.Fatalf("long names must be cut to %d runes, got %d", maxFileNameRunes, len(got))
	}
}

func TestNormalizeOwnerRef(t *testing.T) {
	if ref, ok := NormalizeOwnerRef("  maid:12 "); !ok || *ref != "maid:12" {
		t.Fatalf("got %v %v", ref, ok)
	}

	if ref, ok := NormalizeOwnerRef(""); !ok || ref != nil {
		t.Fatalf("empty ref must be nil, got %v %v", ref, ok)
	}

	if _, ok := NormalizeOwnerRef("a\nb"); ok {
		t.Fatal("control characters must be rejected")
	}

	if _, ok := NormalizeOwnerRef(strings.Repeat("x", 129)); ok {
		t.Fatal("long refs must be rejected")
	}
}

func TestObjectNameAndDisposition(t *testing.T) {
	id := uuid.MustParse("0192f0c4-5d6e-7a00-8000-000000000001")

	if got := ObjectName("hs", "lessons/maid", id, "mp4"); got != "hs/lessons/maid/"+id.String()+".mp4" {
		t.Fatalf("unexpected object name %s", got)
	}

	got := ContentDisposition(true, "Урок 1.mp4")
	want := `inline; filename="____ 1.mp4"; filename*=UTF-8''%D0%A3%D1%80%D0%BE%D0%BA%201.mp4`

	if got != want {
		t.Fatalf("ContentDisposition = %s, want %s", got, want)
	}

	if !strings.HasPrefix(ContentDisposition(false, "a.zip"), "attachment;") {
		t.Fatal("non inline files must be attachments")
	}
}

func TestWithExtension(t *testing.T) {
	cases := []struct {
		name, extension string
		aliases         []string
		want            string
	}{
		{"clip.mp4", "mp4", nil, "clip.mp4"},
		{"Photo.JPEG", "jpg", []string{"jpeg", "jpe"}, "Photo.JPEG"},
		{"invoice", "pdf", nil, "invoice.pdf"},
		{"page.html", "txt", nil, "page.html.txt"},
		{"Lesson 1. Intro", "mp4", nil, "Lesson 1. Intro.mp4"},
	}

	for _, tc := range cases {
		if got := WithExtension(tc.name, tc.extension, tc.aliases...); got != tc.want {
			t.Fatalf("WithExtension(%q, %q) = %q, want %q", tc.name, tc.extension, got, tc.want)
		}
	}

	long := WithExtension(strings.Repeat("я", maxFileNameRunes), "pdf")
	if runes := []rune(long); len(runes) != maxFileNameRunes || !strings.HasSuffix(long, ".pdf") {
		t.Fatalf("long names must keep the extension within %d runes, got %d", maxFileNameRunes, len(runes))
	}
}

func TestContentDispositionEncodesAttrChars(t *testing.T) {
	got := ContentDisposition(false, "a'b(1)*.pdf")
	want := `attachment; filename="a'b(1)*.pdf"; filename*=UTF-8''a%27b%281%29%2A.pdf`

	if got != want {
		t.Fatalf("ContentDisposition = %s, want %s", got, want)
	}
}
