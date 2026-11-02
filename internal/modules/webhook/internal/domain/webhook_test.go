package domain

import "testing"

func TestValidateURLBlocksInternalTargets(t *testing.T) {
	for _, u := range []string{"http://hooks.merchant.et/x", "https://127.0.0.1/x", "https://localhost/x",
		"https://169.254.169.254/latest/meta-data", "https://10.0.0.5/hook", "https://[::1]/x",
		"https://metadata.google.internal/", "https://user:pass@hooks.merchant.et/", "ftp://hooks.merchant.et/"} {
		if ValidateURL(u, false) == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if err := ValidateURL("https://hooks.merchant.et/paygate", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateURL("http://127.0.0.1:9000/hook", true); err != nil {
		t.Fatal("development mode must allow local URLs")
	}
}
