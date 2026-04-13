package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrSkillNotFound = errors.New("skill not found")

type Document struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Registry struct {
	Roots []string
}

func NewRegistry(roots []string) Registry {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root != "" {
			out = append(out, filepath.Clean(root))
		}
	}
	return Registry{Roots: out}
}

func (r Registry) LoadMany(refs []string) ([]Document, error) {
	var out []Document
	seen := map[string]bool{}
	for _, ref := range refs {
		doc, err := r.Load(ref)
		if err != nil {
			return nil, err
		}
		if seen[doc.Path] {
			continue
		}
		seen[doc.Path] = true
		out = append(out, doc)
	}
	return out, nil
}

func (r Registry) Load(ref string) (Document, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Document{}, ErrSkillNotFound
	}
	if path, ok := resolvePathRef(ref); ok {
		return loadDocument(path)
	}

	for _, root := range r.Roots {
		if path, ok := matchNamedSkill(root, ref); ok {
			return loadDocument(path)
		}
	}
	return Document{}, ErrSkillNotFound
}

func resolvePathRef(ref string) (string, bool) {
	if filepath.IsAbs(ref) || strings.ContainsRune(ref, os.PathSeparator) || strings.HasSuffix(strings.ToLower(ref), ".md") {
		path := filepath.Clean(ref)
		info, err := os.Stat(path)
		if err != nil {
			return "", false
		}
		if info.IsDir() {
			path = filepath.Join(path, "SKILL.md")
			if _, err := os.Stat(path); err != nil {
				return "", false
			}
		}
		return path, true
	}
	return "", false
}

func matchNamedSkill(root, name string) (string, bool) {
	name = strings.TrimSpace(name)
	direct := filepath.Join(root, name, "SKILL.md")
	if _, err := os.Stat(direct); err == nil {
		return direct, true
	}

	var matched string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || matched != "" {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() != "SKILL.md" {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == name {
			matched = path
		}
		return nil
	})
	return matched, matched != ""
}

func loadDocument(path string) (Document, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if strings.EqualFold(name, "SKILL") {
		name = filepath.Base(filepath.Dir(path))
	}
	return Document{
		Name:    name,
		Path:    filepath.Clean(path),
		Content: string(bytes),
	}, nil
}
