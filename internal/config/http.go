package config

import (
	"errors"
	"fmt"
	"strconv"
)

const defaultHTTPAddr = "127.0.0.1:8080"

// ResolveHTTPAddr keeps the safe loopback default for ordinary hosts while
// honouring Cloud Run's container contract. Cloud Run does not expand variable
// references inside another environment variable, so MCP_HTTP_ADDR=$PORT would
// be incorrect; the application must consume PORT itself.
func ResolveHTTPAddr(configured string, explicitlySet bool, lookupEnv func(string) (string, bool)) (string, error) {
	if explicitlySet {
		return configured, nil
	}
	if configured == "" {
		configured = defaultHTTPAddr
	}
	if lookupEnv == nil {
		return configured, nil
	}
	if _, cloudRun := lookupEnv("K_SERVICE"); !cloudRun {
		return configured, nil
	}
	port, ok := lookupEnv("PORT")
	if !ok || port == "" {
		return "", errors.New("Cloud Run K_SERVICE is set but PORT is missing") //nolint:staticcheck // Cloud Run and its env names are proper nouns.
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid Cloud Run PORT %q: expected an integer from 1 to 65535", port)
	}
	return ":" + port, nil
}
