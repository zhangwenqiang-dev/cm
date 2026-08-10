package connectmac

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type SkillDiffResult struct {
	Text    string
	Changed bool
}

// Diff compares the installed managed skill files with their built-in templates.
func (m SkillManager) Diff() (SkillDiffResult, error) {
	if _, err := os.Stat(m.SkillPath); os.IsNotExist(err) {
		return SkillDiffResult{}, fmt.Errorf("connectmac skill is not installed at %s; run cm skill install", m.SkillPath)
	} else if err != nil {
		return SkillDiffResult{}, fmt.Errorf("check installed connectmac skill %s: %w", m.SkillPath, err)
	}

	installedSkillPath := filepath.Join(m.SkillPath, "SKILL.md")
	installedSkill, err := os.ReadFile(installedSkillPath)
	if err != nil {
		return SkillDiffResult{}, fmt.Errorf("read installed skill file %s: %w", installedSkillPath, err)
	}

	installedMetadataPath := filepath.Join(m.SkillPath, "agents", "openai.yaml")
	installedMetadata, err := os.ReadFile(installedMetadataPath)
	if err != nil {
		return SkillDiffResult{}, fmt.Errorf("read installed skill file %s: %w", installedMetadataPath, err)
	}

	skillDiff, skillChanged := renderUnifiedTextDiff(
		"installed/SKILL.md",
		"built-in/SKILL.md",
		string(installedSkill),
		DefaultSkillTemplate(),
	)
	metadataDiff, metadataChanged := renderUnifiedTextDiff(
		"installed/agents/openai.yaml",
		"built-in/agents/openai.yaml",
		string(installedMetadata),
		DefaultSkillOpenAIYAML(),
	)

	return SkillDiffResult{
		Text:    skillDiff + metadataDiff,
		Changed: skillChanged || metadataChanged,
	}, nil
}

type skillDiffLine struct {
	text       string
	terminated bool
}

type skillDiffOp struct {
	kind byte
	line skillDiffLine
}

// renderUnifiedTextDiff returns a deterministic, line-oriented unified diff.
func renderUnifiedTextDiff(oldName, newName, oldText, newText string) (string, bool) {
	oldLines := splitSkillDiffLines(oldText)
	newLines := splitSkillDiffLines(newText)
	if linesEqual(oldLines, newLines) {
		return "", false
	}

	ops := skillDiffOps(oldLines, newLines)
	hunks := skillDiffHunks(ops)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	for _, hunk := range hunks {
		oldBefore, newBefore := 0, 0
		for _, op := range ops[:hunk.start] {
			if op.kind != '+' {
				oldBefore++
			}
			if op.kind != '-' {
				newBefore++
			}
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[hunk.start:hunk.end] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := oldBefore+1, newBefore+1
		if oldCount == 0 {
			oldStart = oldBefore
		}
		if newCount == 0 {
			newStart = newBefore
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range ops[hunk.start:hunk.end] {
			out.WriteByte(op.kind)
			out.WriteString(op.line.text)
			out.WriteByte('\n')
			if !op.line.terminated {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return out.String(), true
}

func splitSkillDiffLines(text string) []skillDiffLine {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	lines := make([]skillDiffLine, len(parts))
	for i, part := range parts {
		lines[i] = skillDiffLine{text: part, terminated: i < len(parts)-1 || strings.HasSuffix(text, "\n")}
	}
	return lines
}

func linesEqual(oldLines, newLines []skillDiffLine) bool {
	if len(oldLines) != len(newLines) {
		return false
	}
	for i := range oldLines {
		if oldLines[i] != newLines[i] {
			return false
		}
	}
	return true
}

func skillDiffOps(oldLines, newLines []skillDiffLine) []skillDiffOp {
	rows := len(oldLines) + 1
	cols := len(newLines) + 1
	lcs := make([]int, rows*cols)
	cell := func(i, j int) *int { return &lcs[i*cols+j] }
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				*cell(i, j) = 1 + *cell(i+1, j+1)
			} else if *cell(i+1, j) >= *cell(i, j+1) {
				*cell(i, j) = *cell(i+1, j)
			} else {
				*cell(i, j) = *cell(i, j+1)
			}
		}
	}

	ops := make([]skillDiffOp, 0, len(oldLines)+len(newLines))
	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		switch {
		case i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j]:
			ops = append(ops, skillDiffOp{kind: ' ', line: oldLines[i]})
			i++
			j++
		case i < len(oldLines) && (j == len(newLines) || *cell(i+1, j) >= *cell(i, j+1)):
			ops = append(ops, skillDiffOp{kind: '-', line: oldLines[i]})
			i++
		default:
			ops = append(ops, skillDiffOp{kind: '+', line: newLines[j]})
			j++
		}
	}
	return ops
}

type skillDiffHunk struct {
	start int
	end   int
}

func skillDiffHunks(ops []skillDiffOp) []skillDiffHunk {
	var hunks []skillDiffHunk
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		start := i - 3
		if start < 0 {
			start = 0
		}
		end := i + 4
		if end > len(ops) {
			end = len(ops)
		}
		if len(hunks) > 0 && start <= hunks[len(hunks)-1].end {
			if end > hunks[len(hunks)-1].end {
				hunks[len(hunks)-1].end = end
			}
			continue
		}
		hunks = append(hunks, skillDiffHunk{start: start, end: end})
	}
	return hunks
}
