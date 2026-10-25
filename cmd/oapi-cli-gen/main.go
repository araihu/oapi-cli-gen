package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/araihu/oapi-cli-gen/generator"
	"github.com/spf13/cobra"
)

func command() *cobra.Command {
	root := &cobra.Command{Use: "oapi-cli-gen", Short: "Generate an independent Cobra CLI from OpenAPI using oapi-codegen", SilenceUsage: true, SilenceErrors: true}
	options := generator.Options{}
	var output string
	var check, update bool
	var requires []string
	generate := &cobra.Command{Use: "generate", Short: "Generate typed HTTP builders and a Cobra application", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		options.Requires = map[string]string{}
		for _, requirement := range requires {
			path, version, ok := strings.Cut(requirement, "@")
			if !ok {
				return fmt.Errorf("--require expects module@version")
			}
			options.Requires[path] = version
		}
		if check && update {
			return fmt.Errorf("--check and --update are mutually exclusive")
		}
		result, err := generator.Generate(cmd.Context(), options)
		if err != nil {
			return err
		}
		if check {
			if err := result.Check(output); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "generated files are current")
			return nil
		}
		if err := result.Write(output, update); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "generated "+options.Name+" in "+output)
		return nil
	}}
	flags := generate.Flags()
	flags.StringVar(&options.Spec, "spec", "", "Local OpenAPI 3.0/3.1 YAML or JSON contract")
	flags.StringVar(&options.Name, "name", "", "Generated executable name")
	flags.StringVar(&options.Module, "module", "", "Generated Go module path")
	flags.StringVar(&output, "out", "", "Dedicated generated project directory")
	flags.BoolVar(&check, "check", false, "Reject generated drift without writing files")
	flags.BoolVar(&update, "update", false, "Update unchanged generator-owned files")
	flags.StringArrayVar(&requires, "require", nil, "Pin an extra module used by x-go-type-import (module@version)")
	for _, name := range []string{"spec", "name", "module", "out"} {
		if err := generate.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
	root.AddCommand(generate)
	return root
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
