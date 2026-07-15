package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: versioncheck VERSION_FILE")
		return 2
	}

	contents, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "read VERSION file: %v\n", err)
		return 1
	}

	version := strings.TrimSuffix(string(contents), "\n")
	if !isStrictSemVer(version) {
		fmt.Fprintf(stderr, "invalid SemVer 2.0.0 version: %q\n", version)
		return 1
	}

	fmt.Fprintln(stdout, version)
	return 0
}

func isStrictSemVer(version string) bool {
	coreAndPrerelease, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return false
	}

	core, prerelease, hasPrerelease := strings.Cut(coreAndPrerelease, "-")
	if hasPrerelease && !validIdentifiers(prerelease, true) {
		return false
	}

	parts := strings.Split(core, ".")
	return len(parts) == 3 && isNumericIdentifier(parts[0]) && isNumericIdentifier(parts[1]) && isNumericIdentifier(parts[2])
}

func validIdentifiers(value string, rejectLeadingZeroNumeric bool) bool {
	identifiers := strings.Split(value, ".")
	for _, identifier := range identifiers {
		if identifier == "" || !isAlphaNumericHyphen(identifier) {
			return false
		}
		if rejectLeadingZeroNumeric && !isNumericIdentifier(identifier) && isDigits(identifier) {
			return false
		}
	}
	return true
}

func isNumericIdentifier(value string) bool {
	return value == "0" || (len(value) > 0 && value[0] != '0' && isDigits(value))
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isAlphaNumericHyphen(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '-') {
			return false
		}
	}
	return true
}
