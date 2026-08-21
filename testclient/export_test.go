package testclient

import "net/http"

// The assertion checkers are unexported because they are an implementation
// detail of the Assert methods. They are exported to the package's own external
// test binary here, so that the decision behind each assertion can be tested
// without a failing assertion failing the test that drives it.
// ApplyJSON reports the error the JSON option records for a value it cannot
// encode. Driving it through a request would abort the test, since Do reports
// an unencodable body through testing.TB.
func ApplyJSON(value any) error {
	r := &request{header: http.Header{}}
	JSON(value)(r)
	return r.err
}

var (
	CheckStatus    = (*Response).checkStatus
	CheckHeader    = (*Response).checkHeader
	CheckJSON      = (*Response).checkJSON
	CheckErrorCode = (*Response).checkErrorCode
)
