//go:build !unix

package common

// unlinkMetrics reports nothing off Unix. The requirement it measures — that a
// removed file's name is gone at once — is not one this project's suites make on
// Windows: there a just-executed image or a still-open handle keeps the name a
// moment by design, and clitest.TempDir and Go's own t.TempDir wait that out.
// Reporting 0 would be a number nobody measured. Every other platform off Unix
// already fails fit in freeBytes.
func unlinkMetrics(string) ([]Metric, error) {
	return nil, nil
}
