// Command testclean drops the leftover test databases ("popgres_t_*" and
// "popgres_fx_*") and test roles ("popgres_t_*") of the POPGRES_TEST_URL server.
//
// Run it only when there are no running tests (eg. "make test-clean").
package main

import (
	"fmt"
	"os"

	"github.com/Robernetes/popgres/tests"
)

func main() {
	dropped, err := tests.DropOrphanTestDatabases()
	for _, name := range dropped {
		fmt.Println("dropped", name)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d test database(s) dropped\n", len(dropped))

	roles, err := tests.DropOrphanTestRoles()
	for _, name := range roles {
		fmt.Println("dropped role", name)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d test role(s) dropped\n", len(roles))
}
