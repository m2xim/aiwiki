package main

import (
	"strings"
	"testing"
)

func TestSelectMode(t *testing.T) {
	base := []string{"PATH=/bin", "AIWIKI_DATABASE_URL=x", "AIWIKI_S3_BUCKET=wiki", "AIWIKI_PERSONAL_NAME=n", "AIWIKI_SPACE_QUOTA_BYTES=1"}
	if m, err := selectMode("", append([]string{"AIWIKI_OIDC_ISSUER=https://idp.example.org"}, base...)); err != nil || m != modeAuto {
		t.Fatalf("legacy inference must stay untouched: %q %v", m, err)
	}
	if m, err := selectMode("personal", base); err != nil || m != modePersonal {
		t.Fatalf("personal: %q %v", m, err)
	}
	for _, value := range []string{"Personal", "corporate", "trusted", "personal ", "1", "true"} {
		if _, err := selectMode(value, base); err == nil {
			t.Errorf("AIWIKI_MODE=%q accepted", value)
		}
	}
	for _, name := range []string{"AIWIKI_OIDC_ISSUER", "AIWIKI_OIDC_CLIENT_SECRET", "AIWIKI_OIDC_ACCESS_ROLE", "AIWIKI_KEYCLOAK_ADMIN_URL", "AIWIKI_KEYCLOAK_ADMISSION_CLIENT_SECRET", "AIWIKI_MCP_RESOURCE_URL", "AIWIKI_MCP_INTROSPECTION_CLIENT_ID", "AIWIKI_MCP_INTROSPECTION_CLIENT_SECRET"} {
		for _, value := range []string{"secret-value", ""} {
			_, err := selectMode("personal", append([]string{name + "=" + value}, base...))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q not refused by name: %v", name, value, err)
			} else if value != "" && strings.Contains(err.Error(), value) {
				t.Errorf("value leaked: %v", err)
			}
		}
	}
}
