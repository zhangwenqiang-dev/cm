package connectmac

import (
	"fmt"
	"strings"
	"time"
)

func (a App) runLogs(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "usage: cm logs <list|export|clean> [--output <zip>] [--include-raw]")
		return 2
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(a.Err, "usage: cm logs list")
			return 2
		}
		files, err := a.LogManager.List()
		if err != nil {
			fmt.Fprintf(a.Err, "logs list failed: %v\n", err)
			return 1
		}
		fmt.Fprint(a.Out, FormatLogFiles(files))
		return 0
	case "clean":
		if len(args) != 1 {
			fmt.Fprintln(a.Err, "usage: cm logs clean")
			return 2
		}
		if err := a.LogManager.Clean(30 * 24 * time.Hour); err != nil {
			fmt.Fprintf(a.Err, "logs clean failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(a.Out, "cleaned logs older than 30 days")
		return 0
	case "export":
		options, err := parseLogsExportArgs(args[1:])
		if err != nil {
			fmt.Fprintln(a.Err, err)
			return 2
		}
		options.Retention = 30 * 24 * time.Hour
		options.CMVersion = a.Version
		jobRoot := a.JobManager.Dir
		if strings.TrimSpace(jobRoot) == "" {
			jobRoot = DefaultJobDir
		}
		options.JobRoots = []string{jobRoot}
		path, err := a.LogManager.ExportWithOptions(options)
		if err != nil {
			fmt.Fprintf(a.Err, "logs export failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(a.Out, "exported logs: %s\n", path)
		return 0
	default:
		fmt.Fprintf(a.Err, "unknown logs command %q\n", args[0])
		return 2
	}
}

func parseLogsExportArgs(args []string) (LogExportOptions, error) {
	options := LogExportOptions{}
	outputSeen := false
	includeRawSeen := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--output", "-o":
			if outputSeen {
				return LogExportOptions{}, fmt.Errorf("--output may only be specified once")
			}
			outputSeen = true
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "-") {
				return LogExportOptions{}, fmt.Errorf("--output requires a value")
			}
			options.Destination = args[i]
		case "--include-raw":
			if includeRawSeen {
				return LogExportOptions{}, fmt.Errorf("--include-raw may only be specified once")
			}
			includeRawSeen = true
			options.IncludeRaw = true
		default:
			return LogExportOptions{}, fmt.Errorf("unknown logs export option %q", args[i])
		}
	}
	return options, nil
}

func FormatLogFiles(files []LogFile) string {
	if len(files) == 0 {
		return "No logs.\n"
	}
	rows := [][]string{{"FILE", "SIZE", "UPDATED"}}
	for _, file := range files {
		rows = append(rows, []string{
			file.Name,
			fmt.Sprintf("%d", file.Size),
			file.ModTime.Format(time.RFC3339),
		})
	}
	return strings.TrimRight(formatRows(rows), "\n") + "\n"
}
