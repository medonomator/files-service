package domain

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

const SniffBytes = 512

type verification int

const (
	verifySignature verification = iota
	verifyText
)

type family string

const (
	familyJPEG    family = "jpeg"
	familyPNG     family = "png"
	familyGIF     family = "gif"
	familyWebP    family = "webp"
	familyISOBMFF family = "isobmff"
	familyHEIF    family = "heif"
	familyEBML    family = "ebml"
	familyMP3     family = "mp3"
	familyOgg     family = "ogg"
	familyWAV     family = "wav"
	familyPDF     family = "pdf"
	familyZIP     family = "zip"
	familyOLE     family = "ole"
)

type ContentType struct {
	Extension    string
	Aliases      []string
	Inline       bool
	verification verification
	family       family
}

func media(extension string, family family) ContentType {
	return ContentType{Extension: extension, Inline: true, verification: verifySignature, family: family}
}

func document(extension string, family family) ContentType {
	return ContentType{Extension: extension, verification: verifySignature, family: family}
}

func text(extension string) ContentType {
	return ContentType{Extension: extension, verification: verifyText}
}

func (c ContentType) also(aliases ...string) ContentType {
	c.Aliases = aliases

	return c
}

var heifBrands = map[string]bool{
	"heic": true, "heix": true, "heim": true, "heis": true, "hevc": true, "hevx": true,
	"mif1": true, "msf1": true, "avif": true, "avis": true,
}

var catalog = map[string]ContentType{
	"image/jpeg":                    media("jpg", familyJPEG).also("jpeg", "jpe"),
	"image/png":                     media("png", familyPNG),
	"image/gif":                     media("gif", familyGIF),
	"image/webp":                    media("webp", familyWebP),
	"image/heic":                    media("heic", familyHEIF),
	"image/heif":                    media("heif", familyHEIF).also("heic"),
	"image/avif":                    media("avif", familyHEIF),
	"video/mp4":                     media("mp4", familyISOBMFF).also("m4v"),
	"video/quicktime":               media("mov", familyISOBMFF).also("qt"),
	"video/webm":                    media("webm", familyEBML),
	"audio/mpeg":                    media("mp3", familyMP3),
	"audio/mp4":                     media("m4a", familyISOBMFF),
	"audio/x-m4a":                   media("m4a", familyISOBMFF),
	"audio/ogg":                     media("ogg", familyOgg),
	"audio/wav":                     media("wav", familyWAV),
	"application/pdf":               {Extension: "pdf", Inline: true, verification: verifySignature, family: familyPDF},
	"application/zip":               document("zip", familyZIP),
	"application/msword":            document("doc", familyOLE),
	"application/vnd.ms-excel":      document("xls", familyOLE),
	"application/vnd.ms-powerpoint": document("ppt", familyOLE),
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   document("docx", familyZIP),
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         document("xlsx", familyZIP),
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": document("pptx", familyZIP),
	"text/plain":       text("txt").also("log", "text"),
	"text/csv":         text("csv"),
	"text/markdown":    text("md").also("markdown"),
	"application/json": text("json"),
}

func LookupContentType(contentType string) (ContentType, bool) {
	found, ok := catalog[NormalizeContentType(contentType)]

	return found, ok
}

func NormalizeContentType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(contentType))
}

func MatchesContent(contentType string, head []byte) bool {
	expected, ok := LookupContentType(contentType)
	if !ok || len(head) == 0 {
		return false
	}

	if expected.verification == verifyText {
		return detectFamily(head) == "" && looksLikeText(head)
	}

	return detectFamily(head) == expected.family
}

func detectFamily(head []byte) family {
	switch {
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		return familyJPEG
	case bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return familyPNG
	case bytes.HasPrefix(head, []byte("GIF8")):
		return familyGIF
	case riff(head, "WEBP"):
		return familyWebP
	case riff(head, "WAVE"):
		return familyWAV
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")) && heifBrands[string(head[8:12])]:
		return familyHEIF
	case len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp")):
		return familyISOBMFF
	case bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return familyEBML
	case bytes.HasPrefix(head, []byte("ID3")), len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		return familyMP3
	case bytes.HasPrefix(head, []byte("OggS")):
		return familyOgg
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return familyPDF
	case bytes.HasPrefix(head, []byte{'P', 'K', 0x03, 0x04}), bytes.HasPrefix(head, []byte{'P', 'K', 0x05, 0x06}):
		return familyZIP
	case bytes.HasPrefix(head, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}):
		return familyOLE
	default:
		return ""
	}
}

func riff(head []byte, format string) bool {
	return len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte(format))
}

func looksLikeText(head []byte) bool {
	trimmed := head
	for cut := 0; cut < utf8.UTFMax && len(trimmed) > 0 && !utf8.Valid(trimmed); cut++ {
		trimmed = trimmed[:len(trimmed)-1]
	}

	return utf8.Valid(trimmed) && !bytes.ContainsRune(trimmed, 0)
}
