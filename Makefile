lint:
	golangci-lint run -c ./golangci.yml ./...

# the test databases share the same Postgres server (max_connections) so the
# number of parallel test packages is limited
test:
	go test -p 4 ./... -v --cover

# the whole suite with the parity check of the PocketBase v0.40.6 tests (run by the CI)
test-accept:
	./scripts/ci-test.sh

# drops the leftover popgres_t_* and popgres_fx_* test databases
test-clean:
	go run ./tests/cmd/testclean

jstypes:
	go run ./plugins/jsvm/internal/types/types.go

test-report:
	go test -p 4 ./... -v --cover -coverprofile=coverage.out
	go tool cover -html=coverage.out
