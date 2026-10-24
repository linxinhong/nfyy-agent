package industry

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var ErrInvalid = errors.New("invalid industry pack or submission")

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type KnowledgeType struct {
	ID             string   `yaml:"id" json:"id"`
	Name           string   `yaml:"name" json:"name"`
	RequiredFields []string `yaml:"required_fields" json:"requiredFields"`
}

type Area struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
}

type Pack struct {
	ID               string          `yaml:"id" json:"id"`
	Name             string          `yaml:"name" json:"name"`
	KnowledgeTypes   []KnowledgeType `yaml:"knowledge_types" json:"knowledgeTypes"`
	Areas            []Area          `yaml:"-" json:"areas"`
	RequiredSections []string        `yaml:"required_sections" json:"requiredSections"`
	Confidences      []string        `yaml:"confidences" json:"confidences"`
}

type Submission struct {
	Title       string            `json:"title"`
	Type        string            `json:"type"`
	Author      string            `json:"author"`
	SubmittedAt string            `json:"submittedAt"`
	Area        string            `json:"area"`
	Source      string            `json:"source"`
	Confidence  string            `json:"confidence"`
	Redacted    bool              `json:"redacted"`
	MatterID    string            `json:"matterId,omitempty"`
	Fields      map[string]string `json:"fields"`
	Sections    map[string]string `json:"sections"`
}

type Violation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func Load(files fs.FS, directory string) (Pack, error) {
	if !identifier.MatchString(directory) {
		return Pack{}, ErrInvalid
	}
	data, err := fs.ReadFile(files, directory+"/industry.yaml")
	if err != nil {
		return Pack{}, err
	}
	var pack Pack
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&pack); err != nil {
		return Pack{}, ErrInvalid
	}
	if pack.ID != directory || pack.Name == "" || len(pack.KnowledgeTypes) == 0 || len(pack.RequiredSections) == 0 || len(pack.Confidences) == 0 {
		return Pack{}, ErrInvalid
	}
	taxonomyData, err := fs.ReadFile(files, directory+"/taxonomy.yaml")
	if err != nil {
		return Pack{}, err
	}
	var taxonomy struct {
		Areas []Area `yaml:"areas"`
	}
	taxonomyDecoder := yaml.NewDecoder(strings.NewReader(string(taxonomyData)))
	taxonomyDecoder.KnownFields(true)
	if err := taxonomyDecoder.Decode(&taxonomy); err != nil || len(taxonomy.Areas) == 0 {
		return Pack{}, ErrInvalid
	}
	areaIDs := map[string]bool{}
	for _, area := range taxonomy.Areas {
		if !identifier.MatchString(area.ID) || strings.TrimSpace(area.Name) == "" || areaIDs[area.ID] {
			return Pack{}, ErrInvalid
		}
		areaIDs[area.ID] = true
	}
	pack.Areas = taxonomy.Areas
	for _, section := range pack.RequiredSections {
		if !identifier.MatchString(section) {
			return Pack{}, ErrInvalid
		}
	}
	seen := make(map[string]struct{}, len(pack.KnowledgeTypes))
	for _, kind := range pack.KnowledgeTypes {
		if !identifier.MatchString(kind.ID) || kind.Name == "" || len(kind.RequiredFields) == 0 {
			return Pack{}, ErrInvalid
		}
		for _, field := range kind.RequiredFields {
			if !identifier.MatchString(field) {
				return Pack{}, ErrInvalid
			}
		}
		if _, exists := seen[kind.ID]; exists {
			return Pack{}, ErrInvalid
		}
		seen[kind.ID] = struct{}{}
	}
	return pack, nil
}

func (p Pack) Validate(in Submission) []Violation {
	issues := make([]Violation, 0)
	required := map[string]string{
		"title": in.Title, "author": in.Author, "submittedAt": in.SubmittedAt,
		"area": in.Area, "source": in.Source, "confidence": in.Confidence,
	}
	for _, field := range []string{"title", "author", "submittedAt", "area", "source", "confidence"} {
		if strings.TrimSpace(required[field]) == "" {
			issues = append(issues, Violation{field, "required"})
		}
	}
	if utf8.RuneCountInString(in.Title) > 160 || len(in.Source) > 2000 {
		issues = append(issues, Violation{"title/source", "too long"})
	}
	if !in.Redacted {
		issues = append(issues, Violation{"redacted", "must be confirmed before submission"})
	}
	validConfidence := false
	for _, confidence := range p.Confidences {
		validConfidence = validConfidence || in.Confidence == confidence
	}
	if in.Confidence != "" && !validConfidence {
		issues = append(issues, Violation{"confidence", "unrecognized value"})
	}
	if in.SubmittedAt != "" {
		if _, err := time.Parse("2006-01-02", in.SubmittedAt); err != nil {
			issues = append(issues, Violation{"submittedAt", "expected YYYY-MM-DD"})
		}
	}
	validArea := false
	for _, area := range p.Areas {
		validArea = validArea || in.Area == area.ID || in.Area == area.Name
	}
	if in.Area != "" && !validArea {
		issues = append(issues, Violation{"area", "unrecognized area"})
	}
	var selected *KnowledgeType
	for i := range p.KnowledgeTypes {
		if p.KnowledgeTypes[i].ID == in.Type {
			selected = &p.KnowledgeTypes[i]
			break
		}
	}
	if selected == nil {
		issues = append(issues, Violation{"type", "unrecognized knowledge type"})
	} else {
		for _, field := range selected.RequiredFields {
			if strings.TrimSpace(in.Fields[field]) == "" {
				issues = append(issues, Violation{fmt.Sprintf("fields.%s", field), "required"})
			}
		}
	}
	for _, section := range p.RequiredSections {
		if strings.TrimSpace(in.Sections[section]) == "" {
			issues = append(issues, Violation{fmt.Sprintf("sections.%s", section), "required"})
		}
	}
	return issues
}

func Template(files fs.FS, directory, typeID string) (string, error) {
	pack, err := Load(files, directory)
	if err != nil {
		return "", err
	}
	for _, kind := range pack.KnowledgeTypes {
		if kind.ID == typeID {
			data, err := fs.ReadFile(files, directory+"/templates/"+typeID+".md")
			return string(data), err
		}
	}
	return "", ErrInvalid
}
