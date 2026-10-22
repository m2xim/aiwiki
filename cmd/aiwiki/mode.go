package main

import (
	"fmt"
	"strings"
)

// Run modes. Personal mode is never inferred: it needs AIWIKI_MODE=personal.
const (
	modeAuto     = ""
	modePersonal = "personal"
)

// identityEnvPrefixes are the settings of the OIDC/Keycloak identity model,
// which personal mode (one built-in owner) must not be mixed with.
var identityEnvPrefixes = []string{"AIWIKI_OIDC_", "AIWIKI_KEYCLOAK_", "AIWIKI_MCP_RESOURCE_URL", "AIWIKI_MCP_INTROSPECTION_"}

// selectMode validates AIWIKI_MODE against the environment (KEY=VALUE entries).
// An unknown value is an error, and so is any identity setting in personal mode:
// two identity models must not be mixed by accident. Only names are reported.
func selectMode(value string, environ []string) (string, error) {
	switch value {
	case modeAuto:
		return modeAuto, nil
	case modePersonal:
	default:
		return "", fmt.Errorf("unknown AIWIKI_MODE %q (the only supported value is %q; leave it unset otherwise)", value, modePersonal)
	}
	for _, setting := range environ {
		name, _, _ := strings.Cut(setting, "=")
		for _, prefix := range identityEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				return "", fmt.Errorf("%s must not be set with AIWIKI_MODE=personal (personal mode has no identity provider)", name)
			}
		}
	}
	return modePersonal, nil
}
