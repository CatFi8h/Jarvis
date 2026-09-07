package webapp

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

const testToken = "12345:TEST_TOKEN"

// signInitData builds a valid initData string the way Telegram does.
func signInitData(t *testing.T, fields map[string]string) string {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+fields[k])
	}
	secret := hmacSHA256([]byte(testToken), []byte("WebAppData"))
	hash := hex.EncodeToString(hmacSHA256([]byte(strings.Join(lines, "\n")), secret))

	v := url.Values{}
	for k, val := range fields {
		v.Set(k, val)
	}
	v.Set("hash", hash)
	return v.Encode()
}

func TestValidateInitData(t *testing.T) {
	fields := map[string]string{
		"auth_date": fmt.Sprint(time.Now().Unix()),
		"query_id":  "AAF3xJcJ",
		"user":      `{"id":178954149,"first_name":"Igor","language_code":"en"}`,
	}

	id, err := validateInitData(signInitData(t, fields), testToken, time.Hour)
	if err != nil {
		t.Fatalf("valid initData rejected: %v", err)
	}
	if id != "178954149" {
		t.Fatalf("user id = %q, want 178954149", id)
	}

	// Tampered user field must fail.
	good := signInitData(t, fields)
	bad := strings.Replace(good, "178954149", "999999999", 1)
	if _, err := validateInitData(bad, testToken, time.Hour); err == nil {
		t.Fatal("tampered initData accepted")
	}

	// Wrong bot token must fail.
	if _, err := validateInitData(good, "other:TOKEN", time.Hour); err == nil {
		t.Fatal("initData accepted with wrong token")
	}

	// Expired auth_date must fail.
	fields["auth_date"] = fmt.Sprint(time.Now().Add(-25 * time.Hour).Unix())
	if _, err := validateInitData(signInitData(t, fields), testToken, 12*time.Hour); err == nil {
		t.Fatal("expired initData accepted")
	}

	// Missing hash must fail.
	if _, err := validateInitData("auth_date=1&user=%7B%22id%22%3A1%7D", testToken, time.Hour); err == nil {
		t.Fatal("initData without hash accepted")
	}
}
