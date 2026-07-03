package main

import (
	"strings"
	"testing"
)

// TestTier0_NewPatterns covers the patterns added 2026-06-14: more vendor
// tokens, connection-string credentials, IBAN, and labelled passport/DL.
// Each case asserts a category is (or is NOT) flagged — guarding both
// detection and false positives.
func TestTier0_NewPatterns(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // category that MUST appear; "" = MUST flag nothing new
	}{
		{"gitlab", "token glpat-ABCDEFGHIJKLMNOPQRSTUV here", "gitlab_token"},
		{"npm", "npm_abcdefghijklmnopqrstuvwxyz0123456789", "npm_token"},
		{"huggingface", "hf_abcdefghijklmnopqrstuvwxyzABCDEF", "huggingface_token"},
		{"digitalocean", "dop_v1_" + strings.Repeat("a", 64), "digitalocean_token"},
		{"mailgun", "key-0123456789abcdef0123456789abcdef", "mailgun_key"},
		{"gcp_sa", "svc-bot@my-project.iam.gserviceaccount.com", "gcp_service_account"},
		{"azure", `DefaultEndpoint;AccountKey=abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP1234==`, "azure_storage_key"},
		{"connstr_pg", "postgres://admin:s3cr3tpw@db.internal:5432/app", "connection_string_credentials"},
		{"connstr_mongo", "mongodb+srv://user:pass123@cluster0.mongodb.net", "connection_string_credentials"},
		{"iban_valid", "transfer to GB82WEST12345698765432 today", "iban"},

		// False-positive guards: these must NOT trip the new categories.
		{"plain_url_no_creds", "see https://example.com/docs/page for details", ""},
		{"localhost_no_creds", "service at http://localhost:9911/healthz responded", ""},
		{"random_caps_not_iban", "the value ABCD12EFGHIJKLMNOP was logged", ""},
		{"bare_number_not_passport", "order number 12345678 shipped", ""},
		{"normal_prose", "we discussed the design system and color tokens", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits := scanSensitive(c.text)
			has := map[string]bool{}
			for _, h := range hits {
				has[h] = true
			}
			if c.want != "" && !has[c.want] {
				t.Fatalf("want category %q in hits, got %v", c.want, hits)
			}
			if c.want == "" {
				// Must not flag any of the NEW categories (email/phone from the
				// pre-existing PII tier are allowed — we only guard new ones).
				for _, n := range []string{"gitlab_token", "npm_token", "huggingface_token",
					"digitalocean_token", "mailgun_key", "gcp_service_account", "azure_storage_key",
					"connection_string_credentials", "iban", "passport_or_license"} {
					if has[n] {
						t.Fatalf("false positive: %q flagged %q on %q", c.name, n, c.text)
					}
				}
			}
		})
	}
}

func TestIbanValid(t *testing.T) {
	valid := []string{
		"GB82WEST12345698765432", // canonical test IBAN
		"DE89370400440532013000",
		"FR1420041010050500013M02606",
	}
	for _, v := range valid {
		if !ibanValid(v) {
			t.Errorf("ibanValid(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"GB82WEST12345698765433", // last digit wrong → bad checksum
		"XX00NOTANIBAN",
		"GB82",         // too short
		"1234567890123", // no country letters
	}
	for _, v := range invalid {
		if ibanValid(v) {
			t.Errorf("ibanValid(%q) = true, want false", v)
		}
	}
}

func TestSensitiveLLMToggleDefaultsOn(t *testing.T) {
	// nil bank → enabled (safe default; existing behaviour preserved).
	if !sensitiveLLMClassifierEnabled(nil) {
		t.Fatal("sensitiveLLMClassifierEnabled(nil) = false, want true (default on)")
	}
}
