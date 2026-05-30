package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dockershrink",
	Short: "Optimize Docker images",
	Long:  "dockershrink is a CLI tool for optimizing and shrinking Docker images.",
}

func Execute() error {
	return rootCmd.Execute()
}
