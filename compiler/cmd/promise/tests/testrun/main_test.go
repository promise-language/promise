package testrun

import (
	"os"
	"testing"

	"github.com/promise-language/promise/compiler/cmd/promise/clitest"
)

func TestMain(m *testing.M) { os.Exit(clitest.IsolateHome(m)) }
