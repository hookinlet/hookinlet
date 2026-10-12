// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// secretVars are the environment variables that may be given as a file instead:
// NAME_FILE holds the path of a file whose contents are the value. Container
// platforms mount secrets as files (Docker and Kubernetes secrets, systemd
// credentials), and a file is not in the process's environment, which child
// processes inherit and which tools print.
var secretVars = []string{
	EnvPrefix + "ADMIN_TOKEN",
	EnvPrefix + "MASTER_KEY",
	EnvPrefix + "OLD_MASTER_KEY",
	EnvPrefix + "VERIFIER_SECRET",
}

// maxSecretFile bounds what is read from a secret file; the largest secret is
// 4096 bytes.
const maxSecretFile = 64 << 10

// WithSecretFiles returns getenv extended so that each of the secret variables
// (ADMIN_TOKEN, MASTER_KEY, OLD_MASTER_KEY, VERIFIER_SECRET) can also come from the
// file named by the same variable with _FILE added. One trailing newline is dropped,
// since editors and `echo` add one. Giving both forms is an error, and so is a
// file that cannot be read or is empty. The error names the variable and the path,
// never the contents.
func WithSecretFiles(getenv func(string) string) (func(string) string, error) {
	values := map[string]string{}
	var errs []error
	for _, name := range secretVars {
		path := getenv(name + "_FILE")
		if path == "" {
			continue
		}
		if getenv(name) != "" {
			errs = append(errs, fmt.Errorf("%s and %s_FILE are both set; give one", name, name))
			continue
		}
		v, err := readSecretFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s_FILE: %w", name, err))
			continue
		}
		values[name] = v
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return func(k string) string {
		if v, ok := values[k]; ok {
			return v
		}
		return getenv(k)
	}, nil
}

func readSecretFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the file
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, errors.Unwrap(err))
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	if len(b) > maxSecretFile {
		return "", fmt.Errorf("%s is larger than %d bytes, which is not a secret", path, maxSecretFile)
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}
