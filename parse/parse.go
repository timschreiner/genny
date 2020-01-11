package parse

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"strings"
	"unicode"

	"github.com/iancoleman/strcase"
	"golang.org/x/tools/imports"
)

var header = []byte(`

// This file was automatically generated by genny.
// Any changes will be lost if this file is regenerated.
// see https://github.com/cheekybits/genny

`)

var (
	packageKeyword = []byte("package")
	importKeyword  = []byte("import")
	openBrace      = []byte("(")
	closeBrace     = []byte(")")
	genericPackage = "generic"
	genericType    = "generic.Type"
	genericNumber  = "generic.Number"
	linefeed       = "\r\n"
)
var unwantedLinePrefixes = [][]byte{
	[]byte("//go:generate genny "),
}

func subIntoLiteral(lit, typeTemplate, specificType, specificName string) string {
	if lit == typeTemplate {
		return specificType
	}

	if !strings.Contains(lit, typeTemplate) {
		return lit
	}

	if isStructTag(lit) {
		return subIntoStructTag(lit, typeTemplate, specificType, specificName)
	}

	capitalizedName := wordify(specificType, specificName, true)

	result := strings.Replace(lit, typeTemplate, capitalizedName, -1)

	if strings.HasPrefix(result, capitalizedName) && !isExported(lit) {
		uncapitalizedName := wordify(specificType, specificName, false)

		return strings.Replace(result, capitalizedName, uncapitalizedName, 1)
	}

	return result
}

func subIntoStructTag(lit string, typeTemplate string, specificType string, specificName string) string {
	capitalizedName := wordify(specificType, specificName, true)

	snakeCaseName, ok := cacheSnakeCaseNames[capitalizedName]
	if !ok {
		snakeCaseName = strcase.ToSnake(capitalizedName)
		cacheSnakeCaseNames[capitalizedName] = snakeCaseName
	}

	result := strings.Replace(lit, typeTemplate, snakeCaseName, -1)

	return result
}

func isStructTag(lit string) bool {
	return strings.HasPrefix(lit, "`db:") || strings.HasPrefix(lit, "`json:")
}

func subTypeIntoComment(line, typeTemplate, specificType, specificName string) string {
	var sb strings.Builder

	var subbed string
	for _, w := range strings.Fields(line) {
		sb.WriteString(subIntoLiteral(w, typeTemplate, specificType, specificName))
		sb.WriteString(" ")
	}
	return subbed
}

// Does the heavy lifting of taking a line of our code and
// substituting a type into there for our generic type
func subTypeIntoLine(line, typeTemplate, specificType, specificName string) string {
	src := []byte(line)
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, src, nil, scanner.ScanComments)

	var output strings.Builder

	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		} else if tok == token.COMMENT {
			subbed := subTypeIntoComment(lit, typeTemplate, specificType, specificName)
			output.WriteString(subbed)
		} else if tok.IsLiteral() {
			subbed := subIntoLiteral(lit, typeTemplate, specificType, specificName)
			output.WriteString(subbed)
		} else {
			output.WriteString(tok.String())
		}
		output.WriteString(" ")
	}

	return output.String()
}

// typeSet looks like "KeyType: int, ValueType: string"
func generateSpecific(filename string, in io.ReadSeeker, typeSet map[string]string) ([]byte, error) {

	// ensure we are at the beginning of the file
	_, err := in.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	// parse the source file
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, filename, in, 0)
	if err != nil {
		return nil, &errSource{Err: err}
	}

	// make sure every generic.Type is represented in the types
	// argument.
	for _, decl := range file.Decls {
		switch it := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range it.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				switch tt := ts.Type.(type) {
				case *ast.SelectorExpr:
					if name, identOK := tt.X.(*ast.Ident); identOK {
						if name.Name == genericPackage {
							if _, typesetContains := typeSet[ts.Name.Name]; !typesetContains {
								return nil, &errMissingSpecificType{GenericType: ts.Name.Name}
							}
						}
					}
				}
			}
		}
	}

	_, err = in.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	comment := ""
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {

		line := scanner.Text()

		// does this line contain generic.Type?
		if strings.Contains(line, genericType) || strings.Contains(line, genericNumber) {
			comment = ""
			continue
		}

		for t, specificType := range typeSet {
			var specificName string

			if strings.Contains(specificType, ":") {
				split := strings.Split(specificType, ":")
				specificType = split[0]
				specificName = split[1]
			}

			if strings.Contains(line, t) {
				newLine := subTypeIntoLine(line, t, specificType, specificName)
				line = newLine
			}
		}

		if comment != "" {
			buf.WriteString(makeLine(comment))
			comment = ""
		}

		// is this line a comment?
		// TODO: should we handle /* */ comments?
		if strings.HasPrefix(line, "//") {
			// record this line to print later
			comment = line
			continue
		}

		// write the line
		buf.WriteString(makeLine(line))
	}

	// write it out
	return buf.Bytes(), nil
}

// Generics parses the source file and generates the bytes replacing the
// generic types for the keys map with the specific types (its value).
func Generics(filename, outputFilename, pkgName string, in io.ReadSeeker, typeSets []map[string]string) ([]byte, error) {

	totalOutput := header

	for _, typeSet := range typeSets {

		// generate the specifics
		parsed, err := generateSpecific(filename, in, typeSet)
		if err != nil {
			return nil, err
		}

		totalOutput = append(totalOutput, parsed...)

	}

	// clean up the code line by line
	packageFound := false
	insideImportBlock := false
	var cleanOutputLines []string
	scanner := bufio.NewScanner(bytes.NewReader(totalOutput))
	for scanner.Scan() {

		// end of imports block?
		if insideImportBlock {
			if bytes.HasSuffix(scanner.Bytes(), closeBrace) {
				insideImportBlock = false
			}
			continue
		}

		if bytes.HasPrefix(scanner.Bytes(), packageKeyword) {
			if packageFound {
				continue
			} else {
				packageFound = true
			}
		} else if bytes.HasPrefix(scanner.Bytes(), importKeyword) {
			if bytes.HasSuffix(scanner.Bytes(), openBrace) {
				insideImportBlock = true
			}
			continue
		}

		// check all unwantedLinePrefixes - and skip them
		skipline := false
		for _, prefix := range unwantedLinePrefixes {
			if bytes.HasPrefix(scanner.Bytes(), prefix) {
				skipline = true
				continue
			}
		}

		if skipline {
			continue
		}

		cleanOutputLines = append(cleanOutputLines, makeLine(scanner.Text()))
	}

	cleanOutput := strings.Join(cleanOutputLines, "")

	output := []byte(cleanOutput)
	var err error

	// change package name
	if pkgName != "" {
		output = changePackage(bytes.NewReader([]byte(output)), pkgName)
	}

	// fix the imports
	output, err = imports.Process(outputFilename, output, nil)
	if err != nil {
		return nil, &errImports{Err: err}
	}

	return output, nil
}

func makeLine(s string) string {
	return fmt.Sprintln(strings.TrimRight(s, linefeed))
}

// isAlphaNumeric gets whether the rune is alphanumeric or _.
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordify turns a type into a nice word for function and type
// names etc.

var cacheExportedNames = make(map[string]string)
var cacheUnexportedNames = make(map[string]string)
var cacheSnakeCaseNames = make(map[string]string)

func wordify(s string, name string, exported bool) string {
	if name != "" {
		return name
	}

	cache := cacheExportedNames
	if !exported {
		cache = cacheUnexportedNames
	}

	v, ok := cache[s]
	if ok {
		return v
	}

	v = s
	v = strings.TrimRight(v, "{}")
	v = strings.TrimLeft(v, "*&")
	v = strings.Replace(v, ".", "", -1)

	if exported {
		v = strings.ToUpper(string(v[0])) + v[1:]
	}

	cache[s] = v

	return v
}

func changePackage(r io.Reader, pkgName string) []byte {
	var out bytes.Buffer
	sc := bufio.NewScanner(r)
	done := false

	for sc.Scan() {
		s := sc.Text()

		if !done && strings.HasPrefix(s, "package") {
			parts := strings.Split(s, " ")
			parts[1] = pkgName
			s = strings.Join(parts, " ")
			done = true
		}

		_, _ = fmt.Fprintln(&out, s)
	}
	return out.Bytes()
}

func isExported(lit string) bool {
	if len(lit) == 0 {
		return false
	}
	return unicode.IsUpper(rune(lit[0]))
}
