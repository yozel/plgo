package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed plgo/pl.go
var plgoContent []byte

// ToUnexported changes Exported function name to unexported
func ToUnexported(name string) string {
	return strings.ToLower(name[0:1]) + name[1:]
}

// ModuleWriter writes the tmp module wrapper that will be build to shared object
type ModuleWriter struct {
	PackageName string
	Doc         string
	fset        *token.FileSet
	packageAst  *ast.Package
	functions   []CodeWriter
}

// NewModuleWriter parses the go package and returns the FileSet and AST
func NewModuleWriter(packagePath string) (*ModuleWriter, error) {
	fset := token.NewFileSet()
	// skip _test files in current package
	filtertestfiles := func(fi os.FileInfo) bool {
		if strings.HasSuffix(fi.Name(), "_test.go") {
			return false
		}
		return true
	}

	f, err := parser.ParseDir(fset, packagePath, filtertestfiles, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("Cannot parse package: %w", err)
	}
	if len(f) > 1 {
		return nil, fmt.Errorf("More than one package in %s", packagePath)
	}
	packageAst, ok := f["main"]
	if !ok {
		return nil, fmt.Errorf("No package main in %s", packagePath)
	}
	var packageDoc string
	for _, packageFile := range packageAst.Files {
		packageDoc += packageFile.Doc.Text() + "\n"
	}
	//collect functions from the package
	funcVisitor := new(FuncVisitor)
	ast.Walk(funcVisitor, packageAst)
	if funcVisitor.err != nil {
		return nil, funcVisitor.err
	}
	absPackagePath, err := filepath.Abs(packagePath)
	if err != nil {
		return nil, err
	}
	packageName := filepath.Base(absPackagePath)
	return &ModuleWriter{PackageName: packageName, Doc: packageDoc, fset: fset, packageAst: packageAst, functions: funcVisitor.functions}, nil
}

// WriteModule writes the tmp module wrapper
func (mw *ModuleWriter) WriteModule() (string, error) {
	tempPackagePath, err := buildPath()
	if err != nil {
		return "", fmt.Errorf("Cannot get tempdir: %w", err)
	}
	err = mw.writeUserPackage(tempPackagePath)
	if err != nil {
		return "", err
	}
	err = mw.writeplgo(tempPackagePath)
	if err != nil {
		return "", err
	}
	err = mw.writeExportedMethods(tempPackagePath)
	if err != nil {
		return "", err
	}
	return tempPackagePath, nil
}

func (mw *ModuleWriter) writeUserPackage(tempPackagePath string) error {
	ast.Walk(new(Remover), mw.packageAst)
	packageFile, err := os.Create(filepath.Join(tempPackagePath, "package.go"))
	if err != nil {
		return fmt.Errorf("Cannot write file tempdir: %w", err)
	}
	if err = format.Node(packageFile, mw.fset, ast.MergePackageFiles(mw.packageAst, ast.FilterFuncDuplicates)); err != nil {
		return fmt.Errorf("Cannot format package %w", err)
	}
	err = packageFile.Close()
	if err != nil {
		return fmt.Errorf("Cannot write file tempdir: %w", err)
	}
	return nil
}

func (mw *ModuleWriter) writeplgo(tempPackagePath string) error {
	plgoSource := string(plgoContent)
	plgoSource = "package main\n\n" + plgoSource[12:]
	postgresIncludeDir, err := exec.Command("pg_config", "--includedir-server").CombinedOutput()
	if err != nil {
		return fmt.Errorf("Cannot run pg_config: %w", err)
	}
	postgresIncludeStr := getcorrectpath(string(postgresIncludeDir)) // corrects 8.3 filenames on windows
	plgoSource = strings.Replace(plgoSource, "/usr/include/postgresql/17/server", postgresIncludeStr, 1)

	addOtherIncludesAndLDFLAGS(&plgoSource, postgresIncludeStr) // on mingw windows workarounds

	var funcdec string
	for _, f := range mw.functions {
		funcdec += f.FuncDec()
	}
	plgoSource = strings.Replace(plgoSource, "//{funcdec}", funcdec, 1)
	err = ioutil.WriteFile(filepath.Join(tempPackagePath, "pl.go"), []byte(plgoSource), 0644)
	if err != nil {
		return fmt.Errorf("Cannot write file tempdir: %w", err)
	}
	return nil
}

func (mw *ModuleWriter) writeExportedMethods(tempPackagePath string) error {
	buf := bytes.NewBuffer(nil)
	_, err := buf.WriteString(`package main

/*
#include "postgres.h"
#include "utils/elog.h"
#include "fmgr.h"
extern void elog_error(char* string);
*/
import "C"
`)
	if err != nil {
		return fmt.Errorf("Cannot write file tempdir: %w", err)
	}
	for _, f := range mw.functions {
		f.Code(buf)
	}
	//fmt.Println(buf.String())
	code, err := format.Source(buf.Bytes())
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(tempPackagePath, "methods.go"), code, 0644)
	if err != nil {
		return fmt.Errorf("Cannot write file tempdir: %w", err)
	}
	return nil
}

// WriteSQL writes sql file with commands to create functions in DB
func (mw *ModuleWriter) WriteSQL(tempPackagePath string) error {
	sqlPath := filepath.Join(tempPackagePath, mw.PackageName+"--0.1.sql")
	sqlFile, err := os.Create(sqlPath)
	if err != nil {
		return err
	}
	defer sqlFile.Close()
	sqlFile.WriteString(`-- complain if script is sourced in psql, rather than via CREATE EXTENSION
\echo Use "CREATE EXTENSION ` + mw.PackageName + `" to load this file. \quit
`)
	for _, f := range mw.functions {
		f.SQL(mw.PackageName, sqlFile)
	}
	return nil
}

// WriteControl writes .control file for the new postgresql extension
func (mw *ModuleWriter) WriteControl(path string) error {
	control := []byte(`# ` + mw.PackageName + ` extension
comment = '` + mw.PackageName + ` extension'
default_version = '0.1'
relocatable = true`)
	controlPath := filepath.Join(path, mw.PackageName+".control")
	return ioutil.WriteFile(controlPath, control, 0644)
}

// WriteMakefile writes .control file for the new postgresql extension
func (mw *ModuleWriter) WriteMakefile(path string) error {
	makefile := []byte(`EXTENSION = ` + mw.PackageName + `
DATA = ` + mw.PackageName + `--0.1.sql  # script files to install
# REGRESS = ` + mw.PackageName + `_test     # our test script file (without extension)
MODULES = ` + mw.PackageName + `          # our c module file to build

# postgres build stuff
PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)`)
	makePath := filepath.Join(path, "Makefile")
	return ioutil.WriteFile(makePath, makefile, 0644)
}
