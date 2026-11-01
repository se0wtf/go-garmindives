package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const MinPasswordLength = 15
const MaxPasswordLength = 128

func ValidatePassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < MinPasswordLength || utf8.RuneCountInString(password) > MaxPasswordLength {
		return errors.New("password must contain 15 to 128 characters")
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("password must not consist entirely of whitespace")
	}
	lower := strings.ToLower(password)
	for _, base := range []string{"password", "password1", "1234567890", "123456789", "12345678", "qwerty", "qwertyuiop", "asdfghjkl", "letmein", "welcome", "admin", "administrator", "iloveyou", "changeme", "correcthorsebatterystaple"} {
		if lower == base || (strings.Contains(lower, base) && strings.Trim(strings.ReplaceAll(lower, base, ""), "0123456789!@#$%^&*._- ") == "") {
			return errors.New("password is too common or predictable; choose a different passphrase")
		}
	}
	runes := []rune(password)
	same := true
	for _, r := range runes {
		if r != runes[0] {
			same = false
			break
		}
	}
	if same {
		return errors.New("password is too common or predictable; choose a different passphrase")
	}
	return nil
}
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("cannot generate password salt")
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}
func VerifyPassword(encoded, password string) bool {
	if len(password) > 512 || !utf8.ValidString(password) {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false
	}
	values := make([]uint64, 3)
	for i, prefix := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(params[i], prefix) {
			return false
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(params[i], prefix), 10, 32)
		if err != nil {
			return false
		}
		values[i] = value
	}
	if values[0] < 8*1024 || values[0] > 256*1024 || values[1] < 1 || values[1] > 6 || values[2] < 1 || values[2] > 4 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(values[1]), uint32(values[0]), uint8(values[2]), 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}
