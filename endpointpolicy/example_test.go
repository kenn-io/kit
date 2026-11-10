package endpointpolicy_test

import (
	"fmt"

	"go.kenn.io/kit/endpointpolicy"
)

func ExampleCanonicalEndpoint() {
	endpoint, err := endpointpolicy.CanonicalEndpoint("http://catalog:8080/api/", true, "catalog")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(endpoint)
	// Output: http://catalog:8080/api
}
