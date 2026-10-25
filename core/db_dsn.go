package core

import (
	"net/url"
	"strings"
)

// dsnWithRuntimeParam returns a copy of the provided PostgreSQL connection
// string (URL or keyword/value form) with the specified runtime parameter set
// (e.g. search_path), which the driver sends on connection startup.
func dsnWithRuntimeParam(dsn string, key string, value string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}

		q := u.Query()
		q.Set(key, value)
		u.RawQuery = q.Encode()

		return u.String(), nil
	}

	// keyword/value form (values with spaces must be single quoted)
	escaped := strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\`), `'`, `\'`)

	return strings.TrimSpace(dsn) + " " + key + "='" + escaped + "'", nil
}
