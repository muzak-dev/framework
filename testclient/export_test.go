package testclient

// The assertion checkers are unexported because they are an implementation
// detail of the Assert methods. They are exported to the package's own external
// test binary here, so that the decision behind each assertion can be tested
// without a failing assertion failing the test that drives it.
var (
	CheckStatus    = (*Response).checkStatus
	CheckHeader    = (*Response).checkHeader
	CheckJSON      = (*Response).checkJSON
	CheckErrorCode = (*Response).checkErrorCode
)
