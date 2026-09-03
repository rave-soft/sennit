package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

type LSParams struct {
	Path   string   `json:"path,omitempty" description:"The path to the directory to list (defaults to current working directory)"`
	Ignore []string `json:"ignore,omitempty" description:"List of glob patterns to ignore"`
	Depth  int      `json:"depth,omitempty" description:"The maximum depth to traverse"`
	Cursor string   `json:"cursor,omitempty" description:"Stable continuation token"`
}

// LSPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type LSPermissionsParams = proto.LSPermissionsParams

type NodeType string

const (
	NodeTypeFile      NodeType = "file"
	NodeTypeDirectory NodeType = "directory"
)

type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Type     NodeType    `json:"type"`
	Children []*TreeNode `json:"children,omitempty"`
}

type LSResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	TotalFiles    int  `json:"total_files"`
	Truncated     bool `json:"truncated"`
	// Incomplete is true when part of the tree could not be read (a
	// directory removed mid-walk, a permissions denial, EMFILE/ENFILE on a
	// wide tree, ...) and was silently left out of the results. It is
	// reported separately from Truncated: that flag means the result
	// limit or depth cut the listing short, which is a different fact
	// from part of the tree never having been read at all.
	Incomplete bool   `json:"incomplete,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

const (
	LSToolName = "ls"
	maxLSFiles = 1000
)

//go:embed ls.md.tpl
var lsDescriptionTmpl []byte

var lsDescriptionTpl = template.Must(
	template.New("lsDescription").
		Parse(string(lsDescriptionTmpl)),
)

type lsDescriptionData struct {
	MaxFiles int
}

func lsDescription() string {
	return renderTemplate(lsDescriptionTpl, lsDescriptionData{
		MaxFiles: maxLSFiles,
	})
}

func NewLsTool(permissions permission.Requester, workingDir string, lsConfig config.ToolLs) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		LSToolName,
		lsDescription(),
		func(ctx context.Context, params LSParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			searchPath, err := fsext.Expand(cmp.Or(params.Path, workingDir))
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error expanding path: %v", err)), nil
			}

			searchPath = filepathext.SmartJoin(workingDir, searchPath)

			// Check if directory is outside working directory and request permission if needed
			absSearchPath, resolvedSearchPath, outside, err := resolveWithinWorkdir(workingDir, searchPath)
			if err != nil {
				// Resolving a path is infrastructure, not something the
				// model can act on by rewording its call, so it is a Go
				// error rather than a tool result — see AGENTS.md on the
				// difference.
				return fantasy.ToolResponse{}, fmt.Errorf("resolve path: %w", err)
			}
			if outside {
				sessionID := GetSessionFromContext(ctx)
				if sessionID == "" {
					return fantasy.ToolResponse{}, missingSessionID("accessing directories outside working directory")
				}

				path, description := outsideWorkdirNotice("List directory outside working directory", absSearchPath, resolvedSearchPath)
				resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        path,
					ToolCallID:  call.ID,
					ToolName:    LSToolName,
					Action:      "list",
					Description: description,
					Params:      LSPermissionsParams(params),
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return resp, nil
				}
			}

			output, metadata, err := ListDirectoryTree(searchPath, params, lsConfig)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				metadata,
			), nil
		},
	)
}

func ListDirectoryTree(searchPath string, params LSParams, lsConfig config.ToolLs) (string, LSResponseMetadata, error) {
	if _, err := os.Stat(searchPath); os.IsNotExist(err) {
		return "", LSResponseMetadata{}, fmt.Errorf("path does not exist: %s", searchPath)
	}

	depth, limit := lsConfig.Limits()
	maxFiles := cmp.Or(limit, maxLSFiles)
	effectiveDepth := cmp.Or(params.Depth, depth)
	query := fingerprintPage(canonicalPath(searchPath), strings.Join(params.Ignore, "\x00"), fmt.Sprint(effectiveDepth))
	continuation, err := openPageKeyCursor(params.Cursor, "ls", query)
	if err != nil {
		return "", LSResponseMetadata{}, err
	}
	scan := newPageScan[string](continuation.Last, maxFiles)
	incomplete, err := fsext.VisitDirectory(searchPath, params.Ignore, effectiveDepth, func(path string) { scan.Add(path, path) })
	if err != nil {
		return "", LSResponseMetadata{}, fmt.Errorf("error listing directory: %w", err)
	}
	page, last, truncated, total, generation := scan.Finish()
	if err := finishPageKeyCursor(continuation, generation); err != nil {
		return "", LSResponseMetadata{}, err
	}
	metadata := LSResponseMetadata{NumberOfFiles: len(page), TotalFiles: total, Truncated: truncated, Incomplete: incomplete}
	if truncated {
		metadata.Cursor = makePageKeyCursor("ls", query, generation, last)
	}
	tree := createFileTree(page, searchPath)

	var output string
	if truncated {
		output = fmt.Sprintf("There are more than %d files in the directory. Use a more specific path or use the Glob tool to find specific files. The first %[1]d files and directories are included below.\n", maxFiles)
	}
	if incomplete {
		// A model-recoverable condition, not a Go error (AGENTS.md): part
		// of the tree could not be read (removed mid-walk, permissions,
		// EMFILE/ENFILE, a network mount hiccup), so this listing may be
		// missing entries the model should not assume are absent.
		output += "Part of this directory tree could not be read and its entries are missing from the listing below. A file expected here but not shown may still exist; retry or narrow the path to confirm.\n"
	}
	if depth > 0 {
		// Appended, not assigned: both notes can apply at once, and the
		// assignment threw the truncation warning away — the model was
		// then told the tree was depth-limited but not that it was also
		// cut off at maxFiles.
		output += fmt.Sprintf("The directory tree is shown up to a depth of %d. Use a higher depth and a specific path to see more levels.\n", cmp.Or(params.Depth, depth))
	}
	return output + "\n" + printTree(tree, searchPath), metadata, nil
}

func createFileTree(sortedPaths []string, rootPath string) []*TreeNode {
	root := []*TreeNode{}
	pathMap := make(map[string]*TreeNode)

	for _, path := range sortedPaths {
		relativePath := strings.TrimPrefix(path, rootPath)
		parts := strings.Split(relativePath, string(filepath.Separator))
		currentPath := ""
		var parentPath string

		var cleanParts []string
		for _, part := range parts {
			if part != "" {
				cleanParts = append(cleanParts, part)
			}
		}
		parts = cleanParts

		if len(parts) == 0 {
			continue
		}

		for i, part := range parts {
			if currentPath == "" {
				currentPath = part
			} else {
				currentPath = filepath.Join(currentPath, part)
			}

			if _, exists := pathMap[currentPath]; exists {
				parentPath = currentPath
				continue
			}

			isLastPart := i == len(parts)-1
			isDir := !isLastPart || strings.HasSuffix(relativePath, string(filepath.Separator))
			nodeType := NodeTypeFile
			if isDir {
				nodeType = NodeTypeDirectory
			}
			newNode := &TreeNode{
				Name:     part,
				Path:     currentPath,
				Type:     nodeType,
				Children: []*TreeNode{},
			}

			pathMap[currentPath] = newNode

			if i > 0 && parentPath != "" {
				if parent, ok := pathMap[parentPath]; ok {
					parent.Children = append(parent.Children, newNode)
				}
			} else {
				root = append(root, newNode)
			}

			parentPath = currentPath
		}
	}

	return root
}

func printTree(tree []*TreeNode, rootPath string) string {
	var result strings.Builder

	result.WriteString("- ")
	result.WriteString(filepath.ToSlash(rootPath))
	if rootPath[len(rootPath)-1] != '/' {
		result.WriteByte('/')
	}
	result.WriteByte('\n')

	for _, node := range tree {
		printNode(&result, node, 1)
	}

	return result.String()
}

func printNode(builder *strings.Builder, node *TreeNode, level int) {
	indent := strings.Repeat("  ", level)

	nodeName := node.Name
	if node.Type == NodeTypeDirectory {
		nodeName = nodeName + "/"
	}

	fmt.Fprintf(builder, "%s- %s\n", indent, nodeName)

	if node.Type == NodeTypeDirectory && len(node.Children) > 0 {
		for _, child := range node.Children {
			printNode(builder, child, level+1)
		}
	}
}
