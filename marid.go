package marid

import (
	"fmt"
	"go/format"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

type Marid interface {
	Configuration
	Logr
	Doer
	Templater
}

type Doer interface {
	Do(string, []string) error
}

type Templater interface {
	Render(string, string, interface{}) error
	Fetch(string) (*template.Template, error)
}

type manager struct {
	Configuration
	Logr
	*settings
	*bufferPool
	*LoaderSet
	*BlockSet
	*FuncSet
}

func New(cnf ...Config) Marid {
	m := &manager{
		settings:  defaultSettings(),
		LoaderSet: NewLoaderSet(),
		BlockSet:  NewBlockSet(),
		FuncSet:   NewFuncSet(),
	}
	m.AddLoaders(baseLoader)
	m.Configuration = newConfiguration(m, cnf...)
	return m
}

func (m *manager) render(t *template.Template, d interface{}, dir, file string) error {
	m.PrintIf("rendering template %s...", t.Name())
	b := m.get()

	if xErr := t.Execute(b, d); xErr != nil {
		return RenderError(xErr)
	}

	src, fErr := format.Source(b.Bytes())
	if fErr != nil {
		m.PrintIf("go source format error: %s", fErr.Error())
		m.PrintIf("for provided source:\n%s", b.Bytes())
		return InvalidGoCodeError(fErr)
	}

	output := strings.ToLower(fmt.Sprintf("%s.go", file))
	outputPath := filepath.Join(dir, output)
	if wErr := ioutil.WriteFile(outputPath, src, 0644); wErr != nil {
		return RenderError(wErr)
	}

	m.put(b)
	m.PrintIf("rendered to directory %s, file %s", dir, file)
	return nil
}

func (m *manager) Do(bl string, fl []string) error {
	m.PrintIf("Doing block %s with args %s", bl, fl)
	if blk, err := m.GetBlock(bl); err == nil {
		fls := blk.Flags()
		fpErr := fls.Parse(fl)
		if fpErr != nil {
			return fpErr
		}
		td := NewTemplateData(blk, fls)
		var rErr error
		for _, t := range blk.Templates() {
			tmpl, tfErr := m.Fetch(t)
			if tfErr != nil {
				return tfErr
			}
			rErr = m.render(tmpl, td.Data, blk.Directory(), t)
			if rErr != nil {
				return rErr
			}
		}
		m.PrintIf("block %s finished", bl)
		return nil
	}
	return NoBlockError(bl)
}

func (m *manager) Render(t, dir string, data interface{}) error {
	m.PrintIf("Render called for: %s", t)
	if tmpl, err := m.Fetch(t); err == nil {
		return m.render(tmpl, data, t, dir)
	}
	return NoTemplateError(t)
}

func (m *manager) Fetch(t string) (*template.Template, error) {
	m.PrintIf("Fetch called for %s", t)
	return m.assemble(t)
}

type Node struct {
	Name string
	Src  string
}

var (
	reExtendsTag  *regexp.Regexp = regexp.MustCompile("{{ extends [\"']?([^'\"}']*)[\"']? }}")
	reIncludeTag  *regexp.Regexp = regexp.MustCompile(`{{ include ["']?([^"]*)["']? }}`)
	reDefineTag   *regexp.Regexp = regexp.MustCompile("{{ ?define \"([^\"]*)\" ?\"?([a-zA-Z0-9]*)?\"? ?}}")
	reTemplateTag *regexp.Regexp = regexp.MustCompile("{{ ?template \"([^\"]*)\" ?([^ ]*)? ?}}")
)

func (m *manager) assemble(t string) (*template.Template, error) {
	m.PrintIf("assembling...%s", t)
	stack := []*Node{}

	err := m.add(&stack, t)

	if err != nil {
		return nil, err
	}

	blocks := map[string]string{}
	blockId := 0

	for _, node := range stack {
		var errInReplace error = nil
		node.Src = reIncludeTag.ReplaceAllStringFunc(node.Src, func(raw string) string {
			parsed := reIncludeTag.FindStringSubmatch(raw)
			templatePath := parsed[1]
			subTpl, err := getTemplate(m, templatePath)
			if err != nil {
				errInReplace = err
				return "[error]"
			}
			return subTpl
		})
		if errInReplace != nil {
			return nil, errInReplace
		}
	}

	for _, node := range stack {
		node.Src = reDefineTag.ReplaceAllStringFunc(node.Src, func(raw string) string {
			parsed := reDefineTag.FindStringSubmatch(raw)
			blockName := fmt.Sprintf("BLOCK_%d", blockId)
			blocks[parsed[1]] = blockName

			blockId += 1
			return "{{ define \"" + blockName + "\" }}"
		})
	}

	var rootTemplate *template.Template

	for i, node := range stack {
		node.Src = reTemplateTag.ReplaceAllStringFunc(node.Src, func(raw string) string {
			parsed := reTemplateTag.FindStringSubmatch(raw)
			origName := parsed[1]
			replacedName, ok := blocks[origName]

			dot := "."
			if len(parsed) == 3 && len(parsed[2]) > 0 {
				dot = parsed[2]
			}
			if ok {
				return fmt.Sprintf(`{{ template "%s" %s }}`, replacedName, dot)
			} else {
				return ""
			}
		})

		var thisTemplate *template.Template

		if i == 0 {
			thisTemplate = template.New(node.Name)
			rootTemplate = thisTemplate
		} else {
			thisTemplate = rootTemplate.New(node.Name)
		}

		thisTemplate.Funcs(m.GetFuncs())

		_, err := thisTemplate.Parse(node.Src)
		if err != nil {
			return nil, err
		}
	}

	m.PrintIf("assembled")
	return rootTemplate, nil
}

func getTemplate(m *manager, t string) (string, error) {
	for _, l := range m.GetLoaders() {
		tmpl, err := l.Load(t)
		if err == nil {
			return tmpl, nil
		}
	}
	return "", NoTemplateError(t)
}

func (m *manager) add(stack *[]*Node, t string) error {
	m.PrintIf("adding node %s...", t)
	tplSrc, err := getTemplate(m, t)

	if err != nil {
		return err
	}

	if len(tplSrc) < 1 {
		return EmptyTemplateError(t)
	}

	extendsMatches := reExtendsTag.FindStringSubmatch(tplSrc)
	if len(extendsMatches) == 2 {
		err := m.add(stack, extendsMatches[1])
		if err != nil {
			return err
		}
		tplSrc = reExtendsTag.ReplaceAllString(tplSrc, "")
	}

	node := &Node{
		Name: t,
		Src:  tplSrc,
	}

	*stack = append((*stack), node)

	m.PrintIf("added node")
	return nil
}
