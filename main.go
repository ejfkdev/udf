package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"

	appi18n "github.com/ejfkdev/udf/i18n"
	"github.com/ejfkdev/udf/image"
	"github.com/ejfkdev/udf/types"
)

type cliOptions struct {
	outputParent string
	imageIndex   int
	repoTag      string
	bufferSize   int
	force        bool
	noProgress   bool
	lang         string
}

func main() {
	tr, err := appi18n.New(appi18n.DetectLanguage(os.Args[1:]))
	if err != nil {
		log.Fatal(err)
	}
	if err := newRootCommand(tr).Execute(); err != nil {
		log.Fatal(appi18n.LocalizeError(tr, err))
	}
}

func newRootCommand(tr *appi18n.Manager) *cobra.Command {
	opts := cliOptions{
		imageIndex: -1,
		bufferSize: 1 << 20,
		lang:       tr.Lang(),
	}

	cmd := &cobra.Command{
		Use:           "udf [options] <archive|directory|glob>...",
		Short:         tr.T("cmd_short", nil),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(tr, opts, args)
		},
	}

	cmd.SetHelpTemplate(renderHelpTemplate(tr))
	cmd.Flags().SetInterspersed(true)
	cmd.InitDefaultHelpFlag()
	if helpFlag := cmd.Flags().Lookup("help"); helpFlag != nil {
		helpFlag.Usage = tr.T("flag_help", nil)
	}
	cmd.Flags().StringVarP(&opts.outputParent, "output", "o", "", tr.T("flag_output", nil))
	addImageSelectionFlags(cmd, tr, &opts)
	cmd.Flags().IntVarP(&opts.bufferSize, "buffer-size", "b", 1<<20, tr.T("flag_buffer_size", nil))
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, tr.T("flag_force", nil))
	cmd.Flags().BoolVar(&opts.noProgress, "no-progress", false, tr.T("flag_no_progress", nil))
	cmd.Flags().StringVarP(&opts.lang, "lang", "l", opts.lang, tr.T("flag_lang", nil))

	cmd.Example = strings.Join([]string{
		tr.T("example_1", nil),
		tr.T("example_2", nil),
		tr.T("example_3", nil),
		tr.T("example_4", nil),
		tr.T("example_5", nil),
		tr.T("example_6", nil),
		tr.T("example_7", nil),
		tr.T("example_8", nil),
		tr.T("example_9", nil),
		tr.T("example_10", nil),
		tr.T("example_11", nil),
	}, "\n")

	cmd.AddCommand(newLsCommand(tr), newCpCommand(tr))

	return cmd
}

func addImageSelectionFlags(cmd *cobra.Command, tr *appi18n.Manager, opts *cliOptions) {
	cmd.Flags().IntVarP(&opts.imageIndex, "image-index", "i", opts.imageIndex, tr.T("flag_image_index", nil))
	cmd.Flags().StringVarP(&opts.repoTag, "repo-tag", "t", opts.repoTag, tr.T("flag_repo_tag", nil))
}

func validateSelection(opts cliOptions) error {
	if opts.repoTag != "" && opts.imageIndex >= 0 {
		return appi18n.NewError("err_selection_conflict", nil, nil)
	}
	return nil
}

func newLsCommand(tr *appi18n.Manager) *cobra.Command {
	opts := cliOptions{
		imageIndex: -1,
		lang:       tr.Lang(),
	}

	cmd := &cobra.Command{
		Use:           "ls [options] <archive> [path]",
		Short:         tr.T("cmd_ls_short", nil),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLs(tr, opts, args)
		},
	}

	cmd.SetHelpTemplate(renderSubHelpTemplate(tr))
	cmd.InitDefaultHelpFlag()
	cmd.Flags().SetInterspersed(true)
	addImageSelectionFlags(cmd, tr, &opts)
	cmd.Flags().StringVar(&opts.lang, "lang", opts.lang, tr.T("flag_lang", nil))
	cmd.Example = strings.Join([]string{
		tr.T("example_ls_1", nil),
		tr.T("example_ls_2", nil),
		tr.T("example_ls_3", nil),
		tr.T("example_ls_4", nil),
	}, "\n")

	return cmd
}

func runLs(tr *appi18n.Manager, opts cliOptions, args []string) error {
	if err := validateSelection(opts); err != nil {
		return err
	}

	meta, err := image.ScanImageMetadata(args[0], image.Selection{
		ImageIndex: opts.imageIndex,
		RepoTag:    opts.repoTag,
	})
	if err != nil {
		return appi18n.NewError("err_scan_metadata", nil, err)
	}

	tree, err := image.BuildFileSystem(args[0], meta)
	if err != nil {
		return appi18n.NewError("err_ls_failed", nil, err)
	}

	target := ""
	if len(args) == 2 {
		target = args[1]
	}
	out, err := image.FormatListing(tree, target)
	if err != nil {
		var le *appi18n.LocalizedError
		if errors.As(err, &le) {
			return le
		}
		return appi18n.NewError("err_ls_failed", nil, err)
	}

	fmt.Println(tr.T("msg_ls_archive", map[string]any{"Path": args[0]}))
	fmt.Println(out)
	return nil
}

func newCpCommand(tr *appi18n.Manager) *cobra.Command {
	opts := cliOptions{
		imageIndex: -1,
		bufferSize: 1 << 20,
		lang:       tr.Lang(),
	}

	cmd := &cobra.Command{
		Use:           "cp [options] <archive> <src-path> <dest>",
		Short:         tr.T("cmd_cp_short", nil),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCp(tr, opts, args)
		},
	}

	cmd.SetHelpTemplate(renderSubHelpTemplate(tr))
	cmd.InitDefaultHelpFlag()
	cmd.Flags().SetInterspersed(true)
	addImageSelectionFlags(cmd, tr, &opts)
	cmd.Flags().IntVarP(&opts.bufferSize, "buffer-size", "b", opts.bufferSize, tr.T("flag_buffer_size", nil))
	cmd.Flags().StringVar(&opts.lang, "lang", opts.lang, tr.T("flag_lang", nil))
	cmd.Example = strings.Join([]string{
		tr.T("example_cp_1", nil),
		tr.T("example_cp_2", nil),
		tr.T("example_cp_3", nil),
		tr.T("example_cp_4", nil),
	}, "\n")

	return cmd
}

func runCp(tr *appi18n.Manager, opts cliOptions, args []string) error {
	if err := validateSelection(opts); err != nil {
		return err
	}
	if opts.bufferSize <= 0 {
		return appi18n.NewError("err_invalid_buffer", map[string]any{"Value": opts.bufferSize}, nil)
	}

	meta, err := image.ScanImageMetadata(args[0], image.Selection{
		ImageIndex: opts.imageIndex,
		RepoTag:    opts.repoTag,
	})
	if err != nil {
		return appi18n.NewError("err_scan_metadata", nil, err)
	}

	count, err := image.ExtractPath(args[0], meta, args[1], args[2], opts.bufferSize)
	if err != nil {
		var le *appi18n.LocalizedError
		if errors.As(err, &le) {
			return le
		}
		return appi18n.NewError("err_cp_failed", nil, err)
	}

	fmt.Println(tr.T("msg_cp_done", map[string]any{"Count": count, "Path": args[2]}))
	return nil
}

func run(tr *appi18n.Manager, opts cliOptions, inputs []string) error {
	if err := validateSelection(opts); err != nil {
		return err
	}
	if opts.bufferSize <= 0 {
		return appi18n.NewError("err_invalid_buffer", map[string]any{"Value": opts.bufferSize}, nil)
	}

	imagePaths, batchMode, err := resolveInputs(inputs)
	if err != nil {
		return err
	}
	if len(imagePaths) == 0 {
		return appi18n.NewError("err_no_supported_archive", nil, nil)
	}

	var processed int
	var failed int
	for _, imagePath := range imagePaths {
		ok, err := processImage(imagePath, tr, opts, batchMode)
		if err != nil {
			if batchMode {
				failed++
				fmt.Fprintln(os.Stderr, tr.T("msg_batch_fail", map[string]any{
					"Path":  imagePath,
					"Error": appi18n.LocalizeError(tr, err),
				}))
				continue
			}
			return err
		}
		if ok {
			processed++
		}
	}

	if processed == 0 {
		return appi18n.NewError("err_no_image_archive", nil, nil)
	}
	if processed > 1 {
		fmt.Println(tr.T("msg_batch_processed", map[string]any{"Count": processed}))
	}
	if failed > 0 {
		fmt.Println(tr.T("msg_batch_failed", map[string]any{"Count": failed}))
	}
	return nil
}

func processImage(imagePath string, tr *appi18n.Manager, opts cliOptions, allowSkip bool) (bool, error) {
	meta, err := image.ScanImageMetadata(imagePath, image.Selection{
		ImageIndex: opts.imageIndex,
		RepoTag:    opts.repoTag,
	})
	if err != nil {
		if allowSkip {
			fmt.Fprintln(os.Stderr, tr.T("msg_batch_skip", map[string]any{
				"Path":  imagePath,
				"Error": appi18n.LocalizeError(tr, err),
			}))
			return false, nil
		}
		return false, appi18n.NewError("err_scan_metadata", nil, err)
	}

	outputDir := resolveOutputDir(imagePath, opts.outputParent, meta)

	if err := image.PrepareOutputDir(outputDir, opts.force); err != nil {
		return false, appi18n.NewError("err_prepare_output", map[string]any{"Path": outputDir}, err)
	}
	if err := image.WriteConfigYAML(outputDir, meta); err != nil {
		return false, appi18n.NewError("err_write_config_yaml", map[string]any{"Path": outputDir}, err)
	}

	fmt.Println(tr.T("msg_start_image", map[string]any{"Path": imagePath}))
	fmt.Println(tr.T("msg_image_index", map[string]any{"Value": meta.Index}))
	fmt.Println(tr.T("msg_repo_tags", map[string]any{"Value": fmt.Sprint(meta.RepoTags)}))
	fmt.Println(tr.T("msg_config_path", map[string]any{"Value": meta.ConfigPath}))
	fmt.Println(tr.T("msg_layer_count", map[string]any{"Value": len(meta.LayerOrder)}))
	if meta.Config != nil {
		fmt.Println(tr.T("msg_workdir", map[string]any{"Value": meta.Config.Config.WorkingDir}))
		fmt.Println(tr.T("msg_entrypoint", map[string]any{"Value": fmt.Sprint(meta.Config.Config.Entrypoint)}))
		fmt.Println(tr.T("msg_cmd", map[string]any{"Value": fmt.Sprint(meta.Config.Config.Cmd)}))
	}
	fmt.Println(tr.T("msg_output_dir", map[string]any{"Value": outputDir}))

	progress, err := newProgress(tr, len(meta.LayerOrder), opts.noProgress)
	if err != nil {
		return false, appi18n.NewError("err_init_progress", nil, err)
	}
	if progress != nil {
		defer progress.Close()
	}

	if err := image.ApplyImage(imagePath, meta, outputDir, opts.bufferSize, progress); err != nil {
		return false, appi18n.NewError("err_apply_image", map[string]any{"Path": imagePath}, err)
	}

	if progress != nil {
		progress.MarkDone()
	}
	fmt.Println(tr.T("msg_done", map[string]any{"Path": outputDir}))
	return true, nil
}

func resolveInputs(inputs []string) ([]string, bool, error) {
	seen := make(map[string]struct{})
	var results []string
	var batchMode bool

	for _, input := range inputs {
		paths, expanded, err := expandInput(input)
		if err != nil {
			return nil, false, err
		}
		if expanded || len(inputs) > 1 {
			batchMode = true
		}
		for _, path := range paths {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			results = append(results, path)
		}
	}

	return results, batchMode || len(results) > 1, nil
}

func expandInput(input string) ([]string, bool, error) {
	if hasGlob(input) {
		matches, err := filepath.Glob(input)
		if err != nil {
			return nil, false, fmt.Errorf("解析通配符失败 %s: %w", input, err)
		}
		if len(matches) == 0 {
			return nil, false, fmt.Errorf("通配符未匹配到任何文件: %s", input)
		}
		filtered, err := filterArchives(matches)
		return filtered, true, err
	}

	info, err := os.Stat(input)
	if err != nil {
		return nil, false, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(input)
		if err != nil {
			return nil, false, err
		}
		var paths []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			paths = append(paths, filepath.Join(input, entry.Name()))
		}
		filtered, err := filterArchives(paths)
		return filtered, true, err
	}

	filtered, err := filterArchives([]string{input})
	return filtered, false, err
}

func filterArchives(paths []string) ([]string, error) {
	var filtered []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		if isSupportedArchive(path) {
			filtered = append(filtered, path)
		}
	}
	sort.Strings(filtered)
	return filtered, nil
}

func hasGlob(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

func resolveOutputDir(imagePath, parentDir string, meta *types.ImageMetadata) string {
	if parentDir == "" {
		parentDir = filepath.Dir(imagePath)
	}

	baseDir := filepath.Join(parentDir, imageBaseName(imagePath))
	if meta == nil || meta.Total <= 1 {
		return baseDir
	}

	return filepath.Join(baseDir, imageVariantName(meta))
}

func imageVariantName(meta *types.ImageMetadata) string {
	if meta == nil {
		return "index-0"
	}
	if len(meta.RepoTags) > 0 && strings.TrimSpace(meta.RepoTags[0]) != "" {
		return sanitizeDirName(meta.RepoTags[0])
	}
	return fmt.Sprintf("index-%d", meta.Index)
}

func sanitizeDirName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "untagged"
	}

	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"@", "_",
		" ", "_",
	)
	value = replacer.Replace(value)

	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	result := strings.Trim(b.String(), "._-")
	if result == "" {
		return "untagged"
	}
	return result
}

func imageBaseName(imagePath string) string {
	base := filepath.Base(imagePath)
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		base = base[:len(base)-len(".tar.gz")]
	case strings.HasSuffix(lower, ".tgz"):
		base = base[:len(base)-len(".tgz")]
	case strings.HasSuffix(lower, ".zip"):
		base = base[:len(base)-len(".zip")]
	default:
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if base == "" || base == "." {
		return "rootfs"
	}
	return base
}

type progressReporter struct {
	bar         *progressbar.ProgressBar
	layerPrefix string
	doneText    string
}

func isSupportedArchive(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".tar") ||
		strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".zip")
}

func newProgress(tr *appi18n.Manager, totalLayers int, disabled bool) (*progressReporter, error) {
	if disabled {
		return nil, nil
	}

	bar := progressbar.NewOptions64(
		int64(totalLayers),
		progressbar.OptionSetDescription(tr.T("progress_prefix", nil)),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetWidth(20),
		progressbar.OptionShowCount(),
		progressbar.OptionThrottle(80),
		progressbar.OptionClearOnFinish(),
	)

	return &progressReporter{
		bar:         bar,
		layerPrefix: tr.T("progress_prefix", nil),
		doneText:    tr.T("progress_done", nil),
	}, nil
}

func (p *progressReporter) AddLayer() {
	_ = p.bar.Add(1)
}

func (p *progressReporter) SetLayer(name string) {
	p.bar.Describe(p.layerPrefix + " " + name)
}

func (p *progressReporter) MarkDone() {
	p.bar.Describe(p.doneText)
	_ = p.bar.Finish()
}

func (p *progressReporter) Close() {
	fmt.Fprintln(os.Stderr)
}

func renderHelpTemplate(tr *appi18n.Manager) string {
	return fmt.Sprintf(`%s
  {{.UseLine}}

%s
  %s

%s
{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}{{rpad .Name .NamePadding }} {{.Short}}
{{end}}{{end}}
%s
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}

%s
{{.Example}}

%s
  %s

%s
  %s
`,
		tr.T("help_usage", nil),
		tr.T("help_desc", nil),
		strings.ReplaceAll(tr.T("help_desc_text", nil), "\n", "\n  "),
		tr.T("help_commands", nil),
		tr.T("help_flags", nil),
		tr.T("help_examples", nil),
		tr.T("help_output", nil),
		strings.ReplaceAll(tr.T("help_output_text", nil), "\n", "\n  "),
		tr.T("help_notes", nil),
		strings.ReplaceAll(tr.T("help_notes_text", nil), "\n", "\n  "),
	)
}

func renderSubHelpTemplate(tr *appi18n.Manager) string {
	return fmt.Sprintf(`%s
  {{.UseLine}}

%s
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}

%s
{{.Example}}
`,
		tr.T("help_usage", nil),
		tr.T("help_flags", nil),
		tr.T("help_examples", nil),
	)
}
