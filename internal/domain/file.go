package domain

import (
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusReady   Status = "ready"
	StatusFailed  Status = "failed"
)

const (
	DefaultFolder     = "files"
	stagingPrefix     = "incoming/"
	maxFileNameRunes  = 200
	maxOwnerRefLength = 128
	fallbackFileName  = "file"
)

type File struct {
	ID                uuid.UUID
	ClientID          int64
	Status            Status
	Folder            string
	OwnerRef          *string
	FileName          string
	ObjectName        string
	ContentType       string
	DeclaredSizeBytes int64
	SizeBytes         *int64
	CreatedAt         time.Time
	ReadyAt           *time.Time
}

var folderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}(/[a-z0-9][a-z0-9_-]{0,62}){0,3}$`)

func ValidFolder(folder string) bool {
	return folderPattern.MatchString(folder)
}

func NormalizeOwnerRef(ownerRef string) (*string, bool) {
	trimmed := strings.TrimSpace(ownerRef)
	if trimmed == "" {
		return nil, true
	}

	if len(trimmed) > maxOwnerRefLength || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return nil, false
	}

	return &trimmed, true
}

func CleanFileName(name string) string {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	cleaned := strings.TrimSpace(strings.Map(func(symbol rune) rune {
		if unicode.IsControl(symbol) || symbol == '"' {
			return -1
		}

		return symbol
	}, base))

	if runes := []rune(cleaned); len(runes) > maxFileNameRunes {
		cleaned = string(runes[:maxFileNameRunes])
	}

	if cleaned == "" || cleaned == "." || cleaned == "/" {
		return fallbackFileName
	}

	return cleaned
}

func ObjectName(clientSlug, folder string, id uuid.UUID, extension string) string {
	return clientSlug + "/" + folder + "/" + id.String() + "." + extension
}

func StagingObjectName(id uuid.UUID) string {
	return stagingPrefix + id.String()
}

func WithExtension(fileName, extension string, aliases ...string) string {
	current := strings.ToLower(strings.TrimPrefix(path.Ext(fileName), "."))
	if current == extension {
		return fileName
	}

	for _, alias := range aliases {
		if current == alias {
			return fileName
		}
	}

	stem := fileName
	if runes := []rune(stem); len(runes) > maxFileNameRunes-len(extension)-1 {
		stem = string(runes[:maxFileNameRunes-len(extension)-1])
	}

	return stem + "." + extension
}

func ContentDisposition(inline bool, fileName string) string {
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}

	return disposition + `; filename="` + asciiFallback(fileName) + `"; filename*=UTF-8''` + encodeExtValue(fileName)
}

func asciiFallback(name string) string {
	return strings.Map(func(symbol rune) rune {
		if symbol < 0x20 || symbol > 0x7e || symbol == '"' || symbol == '\\' {
			return '_'
		}

		return symbol
	}, name)
}

func encodeExtValue(value string) string {
	const hex = "0123456789ABCDEF"

	var encoded strings.Builder
	for _, b := range []byte(value) {
		if isAttrChar(b) {
			encoded.WriteByte(b)
			continue
		}

		encoded.WriteByte('%')
		encoded.WriteByte(hex[b>>4])
		encoded.WriteByte(hex[b&0x0f])
	}

	return encoded.String()
}

func isAttrChar(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}

	return strings.IndexByte("!#$&+-.^_`|~", b) >= 0
}
