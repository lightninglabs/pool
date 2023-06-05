package pool

// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/version.go
// Copyright (C) 2015-2019 The Lightning Network Developers

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"

	"google.golang.org/grpc/metadata"
)

// Commit stores the current commit hash of this build, this should be set
// using the -ldflags during compilation.
var Commit string

// semanticAlphabet is the allowed characters from the semantic versioning
// guidelines for pre-release version and build metadata strings. In particular
// they MUST only contain characters in semanticAlphabet.
const semanticAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"

// These constants define the application version and follow the semantic
// versioning 2.0.0 spec (http://semver.org/).
const (
	appMajor uint = 0
	appMinor uint = 6
	appPatch uint = 4

	// appPreRelease MUST only contain characters from semanticAlphabet per
	// the semantic versioning spec.
	appPreRelease = "beta"

	// defaultAgentName is the default name of the software that is added as
	// the first part of the user agent string.
	defaultAgentName = "poold"

	// additionalInfoInitiator is the field name of the initiator in the
	// additional information part of the user agent.
	additionalInfoInitiator = "initiator"
)

// agentName stores the name of the software that is added as the first part of
// the user agent string. This defaults to the value "poold" when being run as
// a standalone component but can be overwritten by LiT for example when poold
// is integrated into the UI.
var agentName = defaultAgentName

// SetAgentName overwrites the default agent name which can be used to identify
// the software Pool is bundled in (for example LiT). This function panics if
// the agent name contains characters outside of the allowed semantic alphabet.
func SetAgentName(newAgentName string) {
	for _, r := range newAgentName {
		if !strings.ContainsRune(semanticAlphabet, r) {
			panic(fmt.Errorf("rune: %v is not in the semantic "+
				"alphabet", r))
		}
	}

	agentName = newAgentName
}

// Version returns the application version as a properly formed string per the
// semantic versioning 2.0.0 spec (http://semver.org/) and the commit it was
// built on.
func Version() string {
	// Append commit hash of current build to version.
	return fmt.Sprintf("%s commit=%s", semanticVersion(), Commit)
}

// semanticVersion returns the SemVer part of the version.
func semanticVersion() string {
	// Start with the major, minor, and patch versions.
	version := fmt.Sprintf("%d.%d.%d", appMajor, appMinor, appPatch)

	// Append pre-release version if there is one. The hyphen called for
	// by the semantic versioning spec is automatically appended and should
	// not be contained in the pre-release string. The pre-release version
	// is not appended if it contains invalid characters.
	preRelease := normalizeVerString(appPreRelease, semanticAlphabet)
	if preRelease != "" {
		version = fmt.Sprintf("%s-%s", version, preRelease)
	}

	return version
}

// UserAgent returns the full user agent string that identifies the software
// that is submitting swaps to the loop server.
func UserAgent(initiator string) string {
	// We'll only allow "safe" characters in the initiator portion of the
	// user agent string and spaces only if surrounded by other characters.
	initiatorAlphabet := semanticAlphabet + ". "
	cleanInitiator := normalizeVerString(
		strings.TrimSpace(initiator), initiatorAlphabet,
	)
	if len(cleanInitiator) > 0 {
		cleanInitiator = fmt.Sprintf(",%s=%s", additionalInfoInitiator,
			cleanInitiator)
	}

	// The whole user agent string is limited to 255 characters server side
	// and also consists of the agent name, version and commit. So we only
	// want to take up at most 150 characters for the initiator. Anything
	// more will just be dropped.
	strLen := len(cleanInitiator)
	cleanInitiator = cleanInitiator[:int(math.Min(float64(strLen), 150))]

	// Assemble full string, including the commit hash of current build.
	return fmt.Sprintf(
		"%s/v%s/commit=%s%s", agentName, semanticVersion(), Commit,
		cleanInitiator,
	)
}

// ContextWithInitiator creates a new context with the given initiator string
// added (provided it is not empty).
func ContextWithInitiator(ctx context.Context,
	initiator string) context.Context {

	trimmed := strings.TrimSpace(initiator)
	if trimmed == "" {
		return ctx
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md = metadata.MD{}
	}

	md[additionalInfoInitiator] = []string{trimmed}
	return metadata.NewIncomingContext(ctx, md)
}

// InitiatorFromContext attempts to read the initiator string from a context and
// returns it if successful. If no initiator string can be found then the empty
// string is returned.
func InitiatorFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}

	initiators := md[additionalInfoInitiator]
	if len(initiators) == 0 {
		return ""
	}

	return initiators[0]
}

// normalizeVerString returns the passed string stripped of all characters
// which are not valid according to the given alphabet.
func normalizeVerString(str, alphabet string) string {
	var result bytes.Buffer
	for _, r := range str {
		if strings.ContainsRune(alphabet, r) {
			result.WriteRune(r)
		}
	}
	return result.String()
}
