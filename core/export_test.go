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

// FindSchemaViewNames returns the names of the findSchemaViews result for the tests.
func FindSchemaViewNames(app App) ([]string, error) {
	views, err := findSchemaViews(app)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(views))
	for i, v := range views {
		names[i] = v.Name
	}

	return names, nil
}
