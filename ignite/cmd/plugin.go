package ignitecmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"

	pluginsconfig "github.com/ignite/cli/ignite/config/plugins"
	"github.com/ignite/cli/ignite/pkg/clictx"
	"github.com/ignite/cli/ignite/pkg/cliui"
	"github.com/ignite/cli/ignite/pkg/cliui/icons"
	"github.com/ignite/cli/ignite/pkg/cosmosanalysis"
	"github.com/ignite/cli/ignite/pkg/xgit"
	"github.com/ignite/cli/ignite/services/plugin"
)

const (
	igniteCmdPrefix   = "ignite "
	flagPluginsGlobal = "global"
)

// plugins hold the list of plugin declared in the config.
// A global variable is used so the list is accessible to the plugin commands.
var plugins []*plugin.Plugin

// LoadPlugins tries to load all the plugins found in configurations.
// If no configurations found, it returns w/o error.
func LoadPlugins(ctx context.Context, rootCmd *cobra.Command) error {
	pluginsConfigs := make([]pluginsconfig.Plugin, 0)

	localCfg, err := parseLocalPlugins(rootCmd)
	if err != nil && !errors.As(err, &cosmosanalysis.ErrPathNotChain{}) {
		return err
	} else if err == nil {
		pluginsConfigs = append(pluginsConfigs, localCfg.Plugins...)
	}

	globalCfg, err := parseGlobalPlugins()
	if err == nil {
		pluginsConfigs = append(pluginsConfigs, globalCfg.Plugins...)
	}

	if len(pluginsConfigs) == 0 {
		return nil
	}

	session := cliui.New(cliui.WithStdout(os.Stdout))
	defer session.End()

	uniquePlugins := plugin.RemoveDuplicates(pluginsConfigs)
	plugins, err = plugin.Load(ctx, uniquePlugins, plugin.CollectEvents(session.EventBus()))
	if err != nil {
		return err
	}
	if len(plugins) == 0 {
		return nil
	}

	return linkPlugins(rootCmd, plugins)
}

func parseLocalPlugins(cmd *cobra.Command) (*pluginsconfig.Config, error) {
	// FIXME(tb): like other commands that works on a chain directory,
	// parseLocalPlugins should rely on `-p` flag to guess that chain directory.
	// Unfortunately parseLocalPlugins is invoked before flags are parsed, so
	// we cannot rely on `-p` flag. As a workaround, we use the working dir.
	// The drawback is we cannot load chain's plugin when using `-p`.
	_ = cmd
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("parse local plugins: %w", err)
	}
	if err := cosmosanalysis.IsChainPath(wd); err != nil {
		return nil, err
	}
	return pluginsconfig.ParseDir(wd)
}

func parseGlobalPlugins() (cfg *pluginsconfig.Config, err error) {
	globalDir, err := plugin.PluginsPath()
	if err != nil {
		return cfg, err
	}

	cfg, err = pluginsconfig.ParseDir(globalDir)
	// if there is error parsing, return empty config and continue execution to load
	// local plugins if they exist.
	if err != nil {
		return &pluginsconfig.Config{}, nil
	}

	for i := range cfg.Plugins {
		cfg.Plugins[i].Global = true
	}
	return
}

func linkPlugins(rootCmd *cobra.Command, plugins []*plugin.Plugin) error {
	// Link plugins to related commands
	var loadErrors []string
	for _, p := range plugins {
		if p.Error != nil {
			loadErrors = append(loadErrors, p.Path)
			continue
		}
		manifest, err := p.Interface.Manifest()
		if err != nil {
			p.Error = fmt.Errorf("Manifest() error: %w", err)
			continue
		}
		linkPluginHooks(rootCmd, p, manifest.Hooks)
		if p.Error != nil {
			loadErrors = append(loadErrors, p.Path)
			continue
		}
		linkPluginCmds(rootCmd, p, manifest.Commands)
		if p.Error != nil {
			loadErrors = append(loadErrors, p.Path)
			continue
		}
	}
	if len(loadErrors) > 0 {
		// unload any plugin that could have been loaded
		defer UnloadPlugins()
		if err := printPlugins(cliui.New(cliui.WithStdout(os.Stdout))); err != nil {
			// content of loadErrors is more important than a print error, so we don't
			// return here, just print the error.
			fmt.Printf("fail to print: %v\n", err)
		}
		return errors.Errorf("fail to load: %v", strings.Join(loadErrors, ","))
	}
	return nil
}

// UnloadPlugins releases any loaded plugins, which is basically killing the
// plugin server instance.
func UnloadPlugins() {
	for _, p := range plugins {
		p.KillClient()
	}
}

func linkPluginHooks(rootCmd *cobra.Command, p *plugin.Plugin, hooks []plugin.Hook) {
	if p.Error != nil {
		return
	}
	for _, hook := range hooks {
		linkPluginHook(rootCmd, p, hook)
	}
}

func linkPluginHook(rootCmd *cobra.Command, p *plugin.Plugin, hook plugin.Hook) {
	cmdPath := hook.PlaceHookOn

	if !strings.HasPrefix(cmdPath, "ignite") {
		// cmdPath must start with `ignite ` before comparison with
		// cmd.CommandPath()
		cmdPath = igniteCmdPrefix + cmdPath
	}

	cmdPath = strings.TrimSpace(cmdPath)

	cmd := findCommandByPath(rootCmd, cmdPath)

	if cmd == nil {
		p.Error = errors.Errorf("unable to find commandPath %q for plugin hook %q", cmdPath, hook.Name)
		return
	}

	if !cmd.Runnable() {
		p.Error = errors.Errorf("can't attach plugin hook %q to non executable command %q", hook.Name, hook.PlaceHookOn)
		return
	}

	newExecutedHook := func(hook plugin.Hook, cmd *cobra.Command, args []string) plugin.ExecutedHook {
		execHook := plugin.ExecutedHook{
			Hook: hook,
			ExecutedCommand: plugin.ExecutedCommand{
				Use:  cmd.Use,
				Path: cmd.CommandPath(),
				Args: args,
				With: p.With,
			},
		}
		execHook.ExecutedCommand.SetFlags(cmd.Flags())
		return execHook
	}

	preRun := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if preRun != nil {
			err := preRun(cmd, args)
			if err != nil {
				return err
			}
		}
		err := p.Interface.ExecuteHookPre(newExecutedHook(hook, cmd, args))
		if err != nil {
			return fmt.Errorf("plugin %q ExecuteHookPre() error: %w", p.Path, err)
		}
		return nil
	}

	runCmd := cmd.RunE

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if runCmd != nil {
			err := runCmd(cmd, args)
			// if the command has failed the `PostRun` will not execute. here we execute the cleanup step before returnning.
			if err != nil {
				err := p.Interface.ExecuteHookCleanUp(newExecutedHook(hook, cmd, args))
				if err != nil {
					fmt.Printf("plugin %q ExecuteHookCleanUp() error: %v", p.Path, err)
				}
			}
			return err
		}

		time.Sleep(100 * time.Millisecond)
		return nil
	}

	postCmd := cmd.PostRunE
	cmd.PostRunE = func(cmd *cobra.Command, args []string) error {
		execHook := newExecutedHook(hook, cmd, args)

		defer func() {
			err := p.Interface.ExecuteHookCleanUp(execHook)
			if err != nil {
				fmt.Printf("plugin %q ExecuteHookCleanUp() error: %v", p.Path, err)
			}
		}()

		if preRun != nil {
			err := postCmd(cmd, args)
			if err != nil {
				// dont return the error, log it and let execution continue to `Run`
				return err
			}
		}

		err := p.Interface.ExecuteHookPost(execHook)
		if err != nil {
			return fmt.Errorf("plugin %q ExecuteHookPost() error : %w", p.Path, err)
		}
		return nil
	}
}

// linkPluginCmds tries to add the plugin commands to the legacy ignite
// commands.
func linkPluginCmds(rootCmd *cobra.Command, p *plugin.Plugin, pluginCmds []plugin.Command) {
	if p.Error != nil {
		return
	}
	for _, pluginCmd := range pluginCmds {
		linkPluginCmd(rootCmd, p, pluginCmd)
		if p.Error != nil {
			return
		}
	}
}

func linkPluginCmd(rootCmd *cobra.Command, p *plugin.Plugin, pluginCmd plugin.Command) {
	cmdPath := pluginCmd.PlaceCommandUnder
	if !strings.HasPrefix(cmdPath, "ignite") {
		// cmdPath must start with `ignite ` before comparison with
		// cmd.CommandPath()
		cmdPath = igniteCmdPrefix + cmdPath
	}
	cmdPath = strings.TrimSpace(cmdPath)

	cmd := findCommandByPath(rootCmd, cmdPath)
	if cmd == nil {
		p.Error = errors.Errorf("unable to find commandPath %q for plugin %q", cmdPath, p.Path)
		return
	}
	if cmd.Runnable() {
		p.Error = errors.Errorf("can't attach plugin command %q to runnable command %q", pluginCmd.Use, cmd.CommandPath())
		return
	}
	for _, cmd := range cmd.Commands() {
		if cmd.Name() == pluginCmd.Use {
			p.Error = errors.Errorf("plugin command %q already exists in ignite's commands", pluginCmd.Use)
			return
		}
	}
	newCmd := &cobra.Command{
		Use:   pluginCmd.Use,
		Short: pluginCmd.Short,
		Long:  pluginCmd.Long,
	}
	for _, f := range pluginCmd.Flags {
		err := f.FeedFlagSet(newCmd.Flags())
		if err != nil {
			p.Error = err
			return
		}
	}
	cmd.AddCommand(newCmd)
	if len(pluginCmd.Commands) == 0 {
		// pluginCmd has no sub commands, so it's runnable
		newCmd.RunE = func(cmd *cobra.Command, args []string) error {
			return clictx.Do(cmd.Context(), func() error {
				execCmd := plugin.ExecutedCommand{
					Use:  cmd.Use,
					Path: cmd.CommandPath(),
					Args: args,
					With: p.With,
				}
				execCmd.SetFlags(cmd.Flags())
				// Call the plugin Execute
				err := p.Interface.Execute(execCmd)
				// NOTE(tb): This pause gives enough time for go-plugin to sync the
				// output from stdout/stderr of the plugin. Without that pause, this
				// output can be discarded and not printed in the user console.
				time.Sleep(100 * time.Millisecond)
				return err
			})
		}
	} else {
		for _, pluginCmd := range pluginCmd.Commands {
			pluginCmd.PlaceCommandUnder = newCmd.CommandPath()
			linkPluginCmd(newCmd, p, pluginCmd)
			if p.Error != nil {
				return
			}
		}
	}
}

func findCommandByPath(cmd *cobra.Command, cmdPath string) *cobra.Command {
	if cmd.CommandPath() == cmdPath {
		return cmd
	}
	for _, cmd := range cmd.Commands() {
		if cmd := findCommandByPath(cmd, cmdPath); cmd != nil {
			return cmd
		}
	}
	return nil
}

// NewPlugin returns a command that groups plugin related sub commands.
func NewPlugin() *cobra.Command {
	c := &cobra.Command{
		Use:   "plugin [command]",
		Short: "Handle plugins",
	}

	c.AddCommand(NewPluginList())
	c.AddCommand(NewPluginUpdate())
	c.AddCommand(NewPluginScaffold())
	c.AddCommand(NewPluginDescribe())
	c.AddCommand(NewPluginAdd())
	c.AddCommand(NewPluginRemove())

	return c
}

func NewPluginList() *cobra.Command {
	lstCmd := &cobra.Command{
		Use:   "list",
		Short: "List declared plugins and status",
		Long:  "Prints status and information of declared plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			s := cliui.New(cliui.WithStdout(os.Stdout))
			return printPlugins(s)
		},
	}
	return lstCmd
}

func NewPluginUpdate() *cobra.Command {
	return &cobra.Command{
		Use:   "update [path]",
		Short: "Update plugins",
		Long:  "Updates a plugin specified by path. If no path is specified all declared plugins are updated",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				// update all plugins
				err := plugin.Update(plugins...)
				if err != nil {
					return err
				}
				fmt.Printf("All plugins updated.\n")
				return nil
			}
			// find the plugin to update
			for _, p := range plugins {
				if p.Path == args[0] {
					err := plugin.Update(p)
					if err != nil {
						return err
					}
					fmt.Printf("Plugin %q updated.\n", p.Path)
					return nil
				}
			}
			return errors.Errorf("Plugin %q not found", args[0])
		},
	}
}

func NewPluginAdd() *cobra.Command {
	cmdPluginAdd := &cobra.Command{
		Use:   "add [path] [key=value]...",
		Short: "Adds a plugin declaration to a plugin configuration",
		Long: `Adds a plugin declaration to a plugin configuration.
Respects key value pairs declared after the plugin path to be added to the
generated configuration definition.
Example:
  ignite plugin add github.com/org/my-plugin/ foo=bar baz=qux`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session := cliui.New(cliui.WithStdout(os.Stdout))
			defer session.End()

			var (
				conf *pluginsconfig.Config
				err  error
			)

			global := flagGetPluginsGlobal(cmd)
			if global {
				conf, err = parseGlobalPlugins()
			} else {
				conf, err = parseLocalPlugins(cmd)
			}
			if err != nil {
				return err
			}

			for _, p := range conf.Plugins {
				if p.Path == args[0] {
					return fmt.Errorf("cannot add duplicate plugin %s", args[0])
				}
			}

			p := pluginsconfig.Plugin{
				Path:   args[0],
				With:   make(map[string]string),
				Global: global,
			}

			pluginsOptions := []plugin.Option{
				plugin.CollectEvents(session.EventBus()),
			}

			var pluginArgs []string
			if len(args) > 1 {
				pluginArgs = args[1:]
			}

			for _, pa := range pluginArgs {
				kv := strings.Split(pa, "=")
				if len(kv) != 2 {
					return fmt.Errorf("malformed key=value arg: %s", pa)
				}
				p.With[kv[0]] = kv[1]
			}

			session.StartSpinner("Loading plugin")
			plugins, err := plugin.Load(cmd.Context(), []pluginsconfig.Plugin{p}, pluginsOptions...)
			if err != nil {
				return err
			}
			if plugins[0].Error != nil {
				return fmt.Errorf("error while loading plugin %q: %w", args[0], plugins[0].Error)
			}
			session.Println("Done loading plugin")
			conf.Plugins = append(conf.Plugins, p)

			if err := conf.Save(); err != nil {
				return err
			}

			session.Printf("🎉 %s added \n", args[0])
			return nil
		},
	}

	cmdPluginAdd.Flags().AddFlagSet(flagSetPluginsGlobal())

	return cmdPluginAdd
}

func NewPluginRemove() *cobra.Command {
	cmdPluginRemove := &cobra.Command{
		Use:     "remove [path]",
		Aliases: []string{"rm"},
		Short:   "Removes a plugin declaration from a chain's plugin configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := cliui.New(cliui.WithStdout(os.Stdout))

			var (
				conf *pluginsconfig.Config
				err  error
			)

			global := flagGetPluginsGlobal(cmd)
			if global {
				conf, err = parseGlobalPlugins()
			} else {
				conf, err = parseLocalPlugins(cmd)
			}
			if err != nil {
				return err
			}

			removed := false
			for i, cp := range conf.Plugins {
				if cp.Path == args[0] {
					conf.Plugins = append(conf.Plugins[:i], conf.Plugins[i+1:]...)
					removed = true
					break
				}
			}

			if !removed {
				// return if no matching plugin path found
				return fmt.Errorf("plugin %s not found", args[0])
			}

			if err := conf.Save(); err != nil {
				return err
			}

			s.Printf("%s %s removed\n", icons.OK, args[0])
			s.Printf("\t%s updated\n", conf.Path())

			return nil
		},
	}

	cmdPluginRemove.Flags().AddFlagSet(flagSetPluginsGlobal())

	return cmdPluginRemove
}

func NewPluginScaffold() *cobra.Command {
	return &cobra.Command{
		Use:   "scaffold [github.com/org/repo]",
		Short: "Scaffold a new plugin",
		Long:  "Scaffolds a new plugin in the current directory with the given repository path configured. A git repository will be created with the given module name, unless the current directory is already a git repository.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session := cliui.New(cliui.StartSpinnerWithText(statusScaffolding))
			defer session.End()

			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			moduleName := args[0]
			path, err := plugin.Scaffold(wd, moduleName)
			if err != nil {
				return err
			}
			if err := xgit.InitAndCommit(path); err != nil {
				return err
			}

			message := `
⭐️ Successfully created a new plugin '%[1]s'.
👉 update plugin code at '%[2]s/main.go'

👉 test plugin integration by adding the following lines in a chain config.yaml:
plugins:
- path: %[2]s

👉 once the plugin is pushed to a repository, replace the local path by the repository path.
`
			session.Printf(message, moduleName, path)
			return nil
		},
	}
}

func NewPluginDescribe() *cobra.Command {
	return &cobra.Command{
		Use:   "describe [path]",
		Short: "Output information about the a registered plugin",
		Long:  "Output information about a registered plugins commands and hooks.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := cliui.New(cliui.WithStdout(os.Stdout))

			for _, p := range plugins {
				if p.Path == args[0] {
					manifest, err := p.Interface.Manifest()
					if err != nil {
						return fmt.Errorf("error while loading plugin manifest: %w", err)
					}

					if err := printPluginCommands(manifest.Commands, s); err != nil {
						return err
					}
					if err := printPluginHooks(manifest.Hooks, s); err != nil {
						return err
					}
					break
				}
			}

			return nil
		},
	}
}

func printPlugins(session *cliui.Session) error {
	var (
		entries     [][]string
		buildStatus = func(p *plugin.Plugin) string {
			if p.Error != nil {
				return fmt.Sprintf("%s Error: %v", icons.NotOK, p.Error)
			}
			manifest, err := p.Interface.Manifest()
			if err != nil {
				return fmt.Sprintf("%s Error: Manifest() returned %v", icons.NotOK, err)
			}
			var (
				hookCount = len(manifest.Hooks)
				cmdCount  = len(manifest.Commands)
			)

			return fmt.Sprintf("%s Loaded: 🪝%d 💻%d", icons.OK, hookCount, cmdCount)
		}
		installedStatus = func(p *plugin.Plugin) string {
			if p.IsGlobal() {
				return "global"
			}
			return "local"
		}
	)
	for _, p := range plugins {
		entries = append(entries, []string{p.Path, buildStatus(p), installedStatus(p)})
	}
	if err := session.PrintTable([]string{"Path", "Status", "Config"}, entries...); err != nil {
		return fmt.Errorf("error while printing plugins: %w", err)
	}
	return nil
}

func printPluginCommands(cmds []plugin.Command, session *cliui.Session) error {
	var entries [][]string
	// Processes command graph
	traverse := func(cmd plugin.Command) {
		// cmdPair is a Wrapper struct to create parent child relationship for sub commands without a `place command under`
		type cmdPair struct {
			cmd    plugin.Command
			parent plugin.Command
		}

		queue := make([]cmdPair, 0)
		queue = append(queue, cmdPair{cmd: cmd, parent: plugin.Command{}})

		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			if c.cmd.PlaceCommandUnder != "" {
				entries = append(entries, []string{c.cmd.Use, c.cmd.PlaceCommandUnder})
			} else {
				entries = append(entries, []string{c.cmd.Use, c.parent.Use})
			}

			for _, sc := range c.cmd.Commands {
				queue = append(queue, cmdPair{cmd: sc, parent: c.cmd})
			}
		}
	}

	for _, c := range cmds {
		traverse(c)
	}

	if err := session.PrintTable([]string{"command use", "under"}, entries...); err != nil {
		return fmt.Errorf("error while printing plugin commands: %w", err)
	}
	return nil
}

func printPluginHooks(hooks []plugin.Hook, session *cliui.Session) error {
	var entries [][]string

	for _, h := range hooks {
		entries = append(entries, []string{h.Name, h.PlaceHookOn})
	}

	if err := session.PrintTable([]string{"hook name", "on"}, entries...); err != nil {
		return fmt.Errorf("error while printing plugin hooks: %w", err)
	}
	return nil
}

func flagSetPluginsGlobal() *flag.FlagSet {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	fs.BoolP(flagPluginsGlobal, "g", false, "use global plugins configuration"+
		" ($HOME/.ignite/plugins/plugins.yml)")
	return fs
}

func flagGetPluginsGlobal(cmd *cobra.Command) bool {
	global, _ := cmd.Flags().GetBool(flagPluginsGlobal)
	return global
}
