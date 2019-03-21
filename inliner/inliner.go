package inliner

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/czlhs/douceur/css"
	"github.com/czlhs/douceur/parser"
	"golang.org/x/net/html"
)

const (
	eltMarkerAttr = "douceur-mark"
)

var unsupportedSelectors = []string{
	":active", ":after", ":before", ":checked", ":disabled", ":enabled",
	":first-line", ":first-letter", ":focus", ":hover", ":invalid", ":in-range",
	":lang", ":link", ":root", ":selection", ":target", ":valid", ":visited"}

// Inliner presents a CSS Inliner
type Inliner struct {
	// Raw HTML
	html string

	// Parsed HTML document
	doc *goquery.Document

	// Parsed stylesheets
	stylesheets []*css.Stylesheet

	// Collected inlinable style rules
	rules []*StyleRule

	// HTML elements matching collected inlinable style rules
	elements map[string]*Element

	// CSS rules that are not inlinable but that must be inserted in output document
	rawRules []fmt.Stringer

	// current element marker value
	eltMarker int

	// fetch external stylesheets, false default not fetch
	fetchExternal bool

	// proxy for fetching css file, only support http
	proxy string

	// base for parse relative path
	base *url.URL
}

// InlineOption Inline option parameter
type InlineOption struct {
	// FetchExternal, whether fetch external css file
	FetchExternal bool
	// SourceURL provide the way to convert relative url to absolute url
	SourceURL string
	// Proxy when fetch external css file, we can use squid to accelate by cache.
	Proxy string
}

// NewInlinerFromReader instanciates a new Inliner
func NewInlinerFromReader(r io.Reader) (*Inliner, error) {
	html, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	inliner := &Inliner{
		html:     string(html),
		elements: make(map[string]*Element),
	}
	return inliner, nil
}

// NewInliner instanciates a new Inliner
func NewInliner(html string) *Inliner {
	inliner := &Inliner{
		html:     html,
		elements: make(map[string]*Element),
	}
	return inliner
}

// Inline inlines css into html document
func Inline(html string) (string, error) {
	doc, err := NewInliner(html).Inline(nil)
	if err != nil {
		return "", err
	}

	newHTML, err := doc.Selection.Html()
	if err != nil {
		return "", err
	}

	return newHTML, nil
}

// Inline inlines CSS and returns HTML
func (inliner *Inliner) Inline(option *InlineOption) (*goquery.Document, error) {
	// parse HTML document
	if err := inliner.resolveOption(option); err != nil {
		return nil, err
	}
	if err := inliner.parseHTML(); err != nil {
		return nil, err
	}

	// parse stylesheets
	if err := inliner.parseStylesheets(); err != nil {
		return nil, err
	}

	// collect elements and style rules
	inliner.collectElementsAndRules()

	// inline css
	if err := inliner.inlineStyleRules(); err != nil {
		return nil, err
	}

	// insert raw stylesheet
	inliner.insertRawStylesheet()

	// generate HTML document
	return inliner.doc, nil
}

// Parses raw html
func (inliner *Inliner) parseHTML() error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(inliner.html))
	if err != nil {
		return err
	}

	inliner.doc = doc

	if inliner.fetchExternal {
		inliner.fetchExternalStyle()
	}

	return nil
}

// Parses and removes stylesheets from HTML document
func (inliner *Inliner) parseStylesheets() error {
	var result error
	inliner.doc.Find("style").EachWithBreak(func(i int, s *goquery.Selection) bool {
		stylesheet, err := parser.Parse(s.Text())
		if err != nil {
			result = err
			fmt.Println(s.Text())
			return false
		}
		inliner.stylesheets = append(inliner.stylesheets, stylesheet)
		// removes parsed stylesheet
		s.Remove()
		return true
	})

	return result
}

// Collects HTML elements matching parsed stylesheets, and thus collect used style rules
func (inliner *Inliner) collectElementsAndRules() {
	for _, stylesheet := range inliner.stylesheets {
		for _, rule := range stylesheet.Rules {
			if rule.Kind == css.QualifiedRule {
				// Let's go!
				inliner.handleQualifiedRule(rule)
			} else {
				// Keep it 'as is'
				inliner.rawRules = append(inliner.rawRules, rule)
			}
		}
	}
}

// Handles parsed qualified rule
func (inliner *Inliner) handleQualifiedRule(rule *css.Rule) {
	for _, selector := range rule.Selectors {
		if Inlinable(selector.Value) {
			inliner.doc.Find(selector.Value).Each(func(i int, s *goquery.Selection) {
				// get marker
				eltMarker, exists := s.Attr(eltMarkerAttr)
				if !exists {
					// mark element
					eltMarker = strconv.Itoa(inliner.eltMarker)
					s.SetAttr(eltMarkerAttr, eltMarker)
					inliner.eltMarker++

					// add new element
					inliner.elements[eltMarker] = NewElement(s)
				}

				// add style rule for element
				inliner.elements[eltMarker].addStyleRule(NewStyleRule(selector.Value, rule.Declarations))
			})
		} else {
			// Keep it 'as is'
			inliner.rawRules = append(inliner.rawRules, NewStyleRule(selector.Value, rule.Declarations))
		}
	}
}

// Inline style rules in HTML document
func (inliner *Inliner) inlineStyleRules() error {
	for _, element := range inliner.elements {
		// remove marker
		element.elt.RemoveAttr(eltMarkerAttr)

		// inline element
		err := element.inline()
		if err != nil {
			return err
		}
	}

	return nil
}

// Computes raw CSS rules
func (inliner *Inliner) computeRawCSS() string {
	result := ""

	for _, rawRule := range inliner.rawRules {
		result += rawRule.String()
		result += "\n"
	}

	return result
}

// Insert raw CSS rules into HTML document
func (inliner *Inliner) insertRawStylesheet() {
	rawCSS := inliner.computeRawCSS()
	if rawCSS != "" {
		// create <style> element
		cssNode := &html.Node{
			Type: html.TextNode,
			Data: "\n" + rawCSS,
		}

		styleNode := &html.Node{
			Type: html.ElementNode,
			Data: "style",
			Attr: []html.Attribute{{Key: "type", Val: "text/css"}},
		}

		styleNode.AppendChild(cssNode)

		// append to <head> element
		headNode := inliner.doc.Find("head")
		if headNode == nil {
			// @todo Create head node !
			panic("NOT IMPLEMENTED: create missing <head> node")
		}

		headNode.AppendNodes(styleNode)
	}
}

func (inliner *Inliner) fetchExternalStyle() (err error) {
	proxyURL, err := url.Parse(inliner.proxy)
	if err != nil {
		return
	}
	httpClinet := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	inliner.doc.Find("link").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if rel, ok := s.Attr("rel"); ok && rel == "stylesheet" {
			cssURL, ok := s.Attr("href")
			if !ok || cssURL == "" {
				return true
			}
			cssURL = toAbsoluteURI(cssURL, inliner.base)
			resp, errTmp := httpClinet.Get(cssURL)
			if err != nil {
				err = errTmp
				return false
			}
			defer resp.Body.Close()
			style, errTmp := ioutil.ReadAll(resp.Body)
			if err != nil {
				err = errTmp
				return false
			}
			s.ReplaceWithHtml(fmt.Sprintf(`<style type="text/css"> %s </style>`, style))
			return true
		}
		return true
	})
	return
}

// Generates HTML
func (inliner *Inliner) genHTML() (string, error) {
	return inliner.doc.Html()
}

// Inlinable returns true if given selector is inlinable
func Inlinable(selector string) bool {
	if strings.Contains(selector, "::") {
		return false
	}

	for _, badSel := range unsupportedSelectors {
		if strings.Contains(selector, badSel) {
			return false
		}
	}

	return true
}

func toAbsoluteURI(url string, base *url.URL) string {
	if base == nil {
		return url
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		if strings.HasPrefix(url, "//") {
			url = base.Scheme + ":" + url
		} else if strings.HasPrefix(url, "/") {
			url = base.Scheme + "://" + base.Host + url
		} else if url != "" {
			if base.Scheme != "" {
				url = base.Scheme + "://" + base.Host + path.Join(base.Path, url)
			} else {
				url = base.Host + path.Join(base.Path, url)
			}
		}
		return url
	}

	return url
}

func (inliner *Inliner) resolveOption(option *InlineOption) error {
	if option == nil {
		return nil
	}

	inliner.fetchExternal = option.FetchExternal
	if option.FetchExternal && option.SourceURL != "" {
		base, err := url.Parse(option.SourceURL)
		if err != nil {
			return err
		}
		inliner.base = base
	}
	if option.Proxy != "" {
		inliner.proxy = option.Proxy
	}

	return nil
}
