// Package ax runs fw inside a google/ax Task container.
//
// It reads the Task and Workspace manifests that ax delivers, prepares each
// workspace once, merges the Workspace settings into the fw configuration,
// supervises spec.command, and reaps orphaned children when fw is PID 1. The
// manifest types follow the YAML schema in pkg/apis/v1alpha1/ax.proto of the
// ax repository. This package does not import the ax module, so the binary
// carries no gRPC dependency for it. See docs/ARCHITECTURE.md section 11.
package ax

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment variables of the ax runner contract.
const (
	EnvTaskYAML         = "AX_TASK_YAML"
	EnvWorkspacesYAML   = "AX_WORKSPACES_YAML"
	EnvMetadataURL      = "AX_METADATA_URL"
	EnvBootstrapTimeout = "AX_BOOTSTRAP_TIMEOUT"
)

// Manifest kinds.
const (
	APIVersion    = "ax.io/v1alpha1"
	KindTask      = "Task"
	KindWorkspace = "Workspace"
)

// ObjectMeta is the metadata of an ax resource.
type ObjectMeta struct {
	Name       string `yaml:"name,omitempty" json:"name,omitempty"`
	Atespace   string `yaml:"atespace,omitempty" json:"atespace,omitempty"`
	CreateTime string `yaml:"createTime,omitempty" json:"createTime,omitempty"`
}

// Task is the ax Task launch configuration. The status is not part of it.
type Task struct {
	APIVersion string     `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
	Kind       string     `yaml:"kind,omitempty" json:"kind,omitempty"`
	Metadata   ObjectMeta `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Spec       TaskSpec   `yaml:"spec,omitempty" json:"spec,omitempty"`
}

// TaskSpec is the spec of a Task.
type TaskSpec struct {
	Image      string         `yaml:"image,omitempty" json:"image,omitempty"`
	Command    []string       `yaml:"command,omitempty" json:"command,omitempty"`
	Env        []EnvVar       `yaml:"env,omitempty" json:"env,omitempty"`
	Resources  ResourcesSpec  `yaml:"resources,omitempty" json:"resources,omitempty"`
	Workspaces []WorkspaceRef `yaml:"workspaces,omitempty" json:"workspaces,omitempty"`
	Debug      bool           `yaml:"debug,omitempty" json:"debug,omitempty"`
}

// EnvVar is one spec.env entry.
type EnvVar struct {
	Name  string `yaml:"name" json:"name"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
}

// ResourcesSpec holds the resource requests and limits of a Task.
type ResourcesSpec struct {
	Requests ResourceList `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   ResourceList `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// ResourceList is a CPU and memory quantity pair, for example "500m" and "1Gi".
type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// WorkspaceRef binds a Workspace to a Task.
type WorkspaceRef struct {
	Name string `yaml:"name" json:"name"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	Goal string `yaml:"goal,omitempty" json:"goal,omitempty"`
}

// Workspace is the ax Workspace resource.
type Workspace struct {
	APIVersion string        `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
	Kind       string        `yaml:"kind,omitempty" json:"kind,omitempty"`
	Metadata   ObjectMeta    `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Spec       WorkspaceSpec `yaml:"spec,omitempty" json:"spec,omitempty"`
}

// WorkspaceSpec is the spec of a Workspace.
type WorkspaceSpec struct {
	Git    []GitRepo    `yaml:"git,omitempty" json:"git,omitempty"`
	Files  []File       `yaml:"files,omitempty" json:"files,omitempty"`
	MCP    MCPConfig    `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	Skills SkillsConfig `yaml:"skills,omitempty" json:"skills,omitempty"`
}

// GitRepo is one repository to clone into the workspace.
type GitRepo struct {
	Name   string `yaml:"name,omitempty" json:"name,omitempty"`
	Repo   string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`
	Dir    string `yaml:"dir,omitempty" json:"dir,omitempty"`
	Depth  int    `yaml:"depth,omitempty" json:"depth,omitempty"`
}

// File is one inline file to write into the workspace.
type File struct {
	Path    string `yaml:"path" json:"path"`
	Content string `yaml:"content,omitempty" json:"content,omitempty"`
}

// MCPConfig lists MCP registries and servers.
type MCPConfig struct {
	Registries []MCPRegistry `yaml:"registries,omitempty" json:"registries,omitempty"`
	Servers    []MCPServer   `yaml:"servers,omitempty" json:"servers,omitempty"`
}

// UnmarshalYAML accepts the legacy form, a bare list of servers, as ax does.
func (m *MCPConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		*m = MCPConfig{}
		return n.Decode(&m.Servers)
	}
	type plain MCPConfig
	return n.Decode((*plain)(m))
}

// MCPRegistry is a registry query for MCP servers.
type MCPRegistry struct {
	Provider string      `yaml:"provider,omitempty" json:"provider,omitempty"`
	Project  string      `yaml:"project,omitempty" json:"project,omitempty"`
	Query    string      `yaml:"query,omitempty" json:"query,omitempty"`
	Servers  []MCPServer `yaml:"servers,omitempty" json:"servers,omitempty"`
}

// MCPServer is one MCP server that the Workspace declares.
type MCPServer struct {
	Name     string   `yaml:"name,omitempty" json:"name,omitempty"`
	Endpoint string   `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Command  string   `yaml:"command,omitempty" json:"command,omitempty"`
	Args     []string `yaml:"args,omitempty" json:"args,omitempty"`
}

// SkillsConfig lists skill registries and the skills directory.
type SkillsConfig struct {
	Registries []SkillRegistry `yaml:"registries,omitempty" json:"registries,omitempty"`
	Path       string          `yaml:"path,omitempty" json:"path,omitempty"`
}

// SkillRegistry is a registry query for skills.
type SkillRegistry struct {
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Project  string `yaml:"project,omitempty" json:"project,omitempty"`
	Query    string `yaml:"query,omitempty" json:"query,omitempty"`
}

// Detect reports whether fw runs as an ax Task, that is, whether
// AX_TASK_YAML is set.
func Detect() bool { return strings.TrimSpace(os.Getenv(EnvTaskYAML)) != "" }

// Identity returns the atespace and the name of the Task. A nil Task gives
// two empty strings.
func Identity(task *Task) (atespace, name string) {
	if task == nil {
		return "", ""
	}
	return task.Metadata.Atespace, task.Metadata.Name
}

// LoadFromEnv reads the Task from AX_TASK_YAML and the Workspaces from the
// multi-document stream in AX_WORKSPACES_YAML. Empty documents are skipped.
// A missing variable is not an error: the Task is then nil, or the list of
// Workspaces is empty, as with the stock runner.
func LoadFromEnv() (*Task, []*Workspace, error) {
	task, err := ParseTask([]byte(os.Getenv(EnvTaskYAML)))
	if err != nil {
		return nil, nil, fmt.Errorf("ax: %s: %w", EnvTaskYAML, err)
	}
	ws, err := ParseWorkspaces([]byte(os.Getenv(EnvWorkspacesYAML)))
	if err != nil {
		return nil, nil, fmt.Errorf("ax: %s: %w", EnvWorkspacesYAML, err)
	}
	return task, ws, nil
}

// LoadFromFiles reads the manifests from files, for local testing. It has the
// semantics of the --task-file and --workspace-file flags of the stock runner:
// an empty taskFile falls back to AX_TASK_YAML, no workspaceFiles fall back to
// AX_WORKSPACES_YAML, and each Workspace file holds one or more documents.
//
// It also accepts a combined manifest file such as examples/ax/reviewer.yaml
// for both flags: the Task is the first document of kind Task, and documents
// of another kind are skipped in a Workspace file.
func LoadFromFiles(taskFile string, workspaceFiles []string) (*Task, []*Workspace, error) {
	var task *Task
	var err error
	if taskFile != "" {
		data, rerr := os.ReadFile(taskFile)
		if rerr != nil {
			return nil, nil, fmt.Errorf("ax: read task file: %w", rerr)
		}
		task, err = ParseTask(data)
		if err != nil {
			return nil, nil, fmt.Errorf("ax: task file %s: %w", taskFile, err)
		}
	} else {
		task, err = ParseTask([]byte(os.Getenv(EnvTaskYAML)))
		if err != nil {
			return nil, nil, fmt.Errorf("ax: %s: %w", EnvTaskYAML, err)
		}
	}
	if len(workspaceFiles) == 0 {
		ws, err := ParseWorkspaces([]byte(os.Getenv(EnvWorkspacesYAML)))
		if err != nil {
			return nil, nil, fmt.Errorf("ax: %s: %w", EnvWorkspacesYAML, err)
		}
		return task, ws, nil
	}
	var all []*Workspace
	for _, f := range workspaceFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("ax: read workspace file: %w", err)
		}
		ws, err := ParseWorkspaces(data)
		if err != nil {
			return nil, nil, fmt.Errorf("ax: workspace file %s: %w", f, err)
		}
		all = append(all, ws...)
	}
	return task, all, nil
}

// ParseTask decodes the first Task in a YAML stream. A document with no kind
// counts as a Task. An empty stream gives a nil Task and no error.
func ParseTask(data []byte) (*Task, error) {
	docs, err := documents(data)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if k := kindOf(d); k != "" && k != KindTask {
			continue
		}
		var t Task
		if err := d.Decode(&t); err != nil {
			return nil, fmt.Errorf("decode task: %w", err)
		}
		return &t, nil
	}
	return nil, nil
}

// ParseWorkspaces decodes every Workspace in a multi-document YAML stream, in
// order. Empty documents and documents of another kind are skipped. A
// document with no kind counts as a Workspace.
func ParseWorkspaces(data []byte) ([]*Workspace, error) {
	docs, err := documents(data)
	if err != nil {
		return nil, err
	}
	var out []*Workspace
	for _, d := range docs {
		if k := kindOf(d); k != "" && k != KindWorkspace {
			continue
		}
		var ws Workspace
		if err := d.Decode(&ws); err != nil {
			return nil, fmt.Errorf("decode workspace: %w", err)
		}
		out = append(out, &ws)
	}
	return out, nil
}

// documents splits a YAML stream into its non-empty documents.
func documents(data []byte) ([]*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
		if isEmptyDocument(&n) {
			continue
		}
		out = append(out, &n)
	}
}

func isEmptyDocument(n *yaml.Node) bool {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return true
		}
		n = n.Content[0]
	}
	switch n.Kind {
	case 0:
		return true
	case yaml.ScalarNode:
		return n.Tag == "!!null"
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) == 0
	}
	return false
}

// kindOf returns the top-level "kind" value of a document, or "".
func kindOf(n *yaml.Node) string {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "kind" {
			return n.Content[i+1].Value
		}
	}
	return ""
}
