package initcmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/urizennnn/switch/internal/config"
)

// Run creates the project configuration and starter rules file.
func Run(args []string) int {
	project := "."
	if len(args) > 0 && args[0] != "" {
		project = args[0]
	}
	project, err := filepath.Abs(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return 1
	}

	created, err := config.EnsureProject(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return 1
	}
	if len(created) == 0 {
		fmt.Printf("switch is already initialized in %s\n", project)
		return 0
	}
	fmt.Printf("initialized switch in %s\n", project)
	for _, path := range created {
		fmt.Printf("  created %s\n", path)
	}
	fmt.Println("\nnext: switch chat")
	return 0
}
