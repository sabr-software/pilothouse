package policy

import (
	"os"

	"github.com/sabr-software/pilothouse/internal/paths"
)

// Check parses and compiles policy file contents exactly as Load would,
// returning the number of rules or a FileErrors with every problem.
func Check(file string, data []byte) (int, error) {
	rules, err := compilePolicy(file, data, paths.UserHome())
	return len(rules), err
}

// CheckFile is Check on a file read from disk.
func CheckFile(file string) (int, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	return Check(file, data)
}
