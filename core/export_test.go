package core

// SetTestDatabaseURLFunc sets the tests only hook that provides a new
// database for the apps created without a connection string.
func SetTestDatabaseURLFunc(f func() string) {
	testDatabaseURLFunc = f
}

// NormalizeViewQueryId exports normalizeViewQueryId for the tests.
func NormalizeViewQueryId(app App, query string) (string, error) {
	return normalizeViewQueryId(app, query)
}
