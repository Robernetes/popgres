lint:
	golangci-lint run -c ./golangci.yml ./...

# the test databases share the same Postgres server (max_connections) so the
# number of parallel test packages is limited
test:
	go test -p 4 ./... -v --cover

# acceptance tests of the current phase (run by the CI until the full suite passes on Postgres)
test-accept:
	go test -count=1 -p 4 ./tests/e2e/... ./tests/ ./tools/dbutils/
	go test -count=1 ./core/validators/ -run '^TestNormalizeUniqueIndexError$$'
	go test -count=1 ./core/ -run '^(TestDSNWithRuntimeParam|TestBaseLockRetry|TestBaseLockRetryInFailedTransaction|TestSQLState|TestGetDefaultRetryInterval)$$'

# drops the leftover popgres_t_* and popgres_fx_* test databases
test-clean:
	go run ./tests/cmd/testclean

jstypes:
	go run ./plugins/jsvm/internal/types/types.go

test-report:
	go test -p 4 ./... -v --cover -coverprofile=coverage.out
	go tool cover -html=coverage.out
