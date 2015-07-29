// Copyright 2015 The go-python Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bind

import (
	"fmt"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/types"
)

const (
	cPreamble = `/*
  C stubs for package %[1]s.
  gopy gen -lang=python %[1]s

  File is generated by gopy gen. Do not edit.
*/

#ifdef _POSIX_C_SOURCE
#undef _POSIX_C_SOURCE
#endif

#include "Python.h"
#include "structmember.h"

// header exported from 'go tool cgo'
#include "%[3]s.h"

`
)

type cpyGen struct {
	decl *printer
	impl *printer

	fset *token.FileSet
	pkg  *Package
	err  ErrorList
}

func (g *cpyGen) gen() error {

	g.genPreamble()

	// first, process structs
	for _, s := range g.pkg.structs {
		g.genStruct(s)
	}

	for _, f := range g.pkg.funcs {
		g.genFunc(f)
	}

	g.impl.Printf("static PyMethodDef cpy_%s_methods[] = {\n", g.pkg.pkg.Name())
	g.impl.Indent()
	for _, f := range g.pkg.funcs {
		name := f.GoName()
		//obj := scope.Lookup(name)
		g.impl.Printf("{%[1]q, %[2]s, METH_VARARGS, %[3]q},\n",
			name, "gopy_"+f.ID(), f.Doc(),
		)
	}
	g.impl.Printf("{NULL, NULL, 0, NULL}        /* Sentinel */\n")
	g.impl.Outdent()
	g.impl.Printf("};\n\n")

	g.impl.Printf("PyMODINIT_FUNC\ninit%[1]s(void)\n{\n", g.pkg.pkg.Name())
	g.impl.Indent()
	g.impl.Printf("PyObject *module = NULL;\n\n")

	for _, s := range g.pkg.structs {
		g.impl.Printf(
			"if (PyType_Ready(&_gopy_%sType) < 0) { return; }\n",
			s.ID(),
		)
	}

	g.impl.Printf("module = Py_InitModule3(%[1]q, cpy_%[1]s_methods, %[2]q);\n\n",
		g.pkg.pkg.Name(),
		g.pkg.doc.Doc,
	)

	for _, s := range g.pkg.structs {
		g.impl.Printf("Py_INCREF(&_gopy_%sType);\n", s.ID())
		g.impl.Printf("PyModule_AddObject(module, %q, (PyObject*)&_gopy_%sType);\n\n",
			s.GoName(),
			s.ID(),
		)
	}
	g.impl.Outdent()
	g.impl.Printf("}\n\n")

	if len(g.err) > 0 {
		return g.err
	}

	return nil
}

func (g *cpyGen) genFunc(o Func) {

	g.impl.Printf(`
/* pythonization of: %[1]s.%[2]s */
static PyObject*
gopy_%[3]s(PyObject *self, PyObject *args) {
`,
		g.pkg.pkg.Name(),
		o.GoName(),
		o.ID(),
	)

	g.impl.Indent()
	g.genFuncBody(o)
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genFuncBody(f Func) {
	id := f.ID()
	sig := f.Signature()

	funcArgs := []string{}

	res := sig.Results()
	args := sig.Params()
	var recv *Var
	if sig.Recv() != nil {
		recv = sig.Recv()
		recv.genRecvDecl(g.impl)
		funcArgs = append(funcArgs, recv.getFuncArg())
	}

	for _, arg := range args {
		arg.genDecl(g.impl)
		funcArgs = append(funcArgs, arg.getFuncArg())
	}

	// FIXME(sbinet) pythonize (turn errors into python exceptions)
	if len(res) > 0 {
		switch len(res) {
		case 1:
			ret := res[0]
			ret.genRetDecl(g.impl)
		default:
			g.impl.Printf("struct %[1]s_return c_gopy_ret;\n", id)
			/*
					for i := 0; i < res.Len(); i++ {
						ret := res.At(i)
						n := ret.Name()
						if n == "" {
							n = "gopy_" + strconv.Itoa(i)
						}
						g.impl.Printf("%[1]s c_%[2]s;\n", ctypeName(ret.Type()), n)
				    }
			*/
		}
	}

	g.impl.Printf("\n")

	if recv != nil {
		recv.genRecvImpl(g.impl)
	}

	if len(args) > 0 {
		g.impl.Printf("if (!PyArg_ParseTuple(args, ")
		format := []string{}
		pyaddrs := []string{}
		for _, arg := range args {
			pyfmt, addr := arg.getArgParse()
			format = append(format, pyfmt)
			pyaddrs = append(pyaddrs, addr)
		}
		g.impl.Printf("%q, %s)) {\n", strings.Join(format, ""), strings.Join(pyaddrs, ", "))
		g.impl.Indent()
		g.impl.Printf("return NULL;\n")
		g.impl.Outdent()
		g.impl.Printf("}\n\n")
	}

	if len(args) > 0 {
		for _, arg := range args {
			arg.genFuncPreamble(g.impl)
		}
		g.impl.Printf("\n")
	}

	if len(res) > 0 {
		g.impl.Printf("c_gopy_ret = ")
	}
	g.impl.Printf("GoPy_%[1]s(%[2]s);\n", id, strings.Join(funcArgs, ", "))

	g.impl.Printf("\n")

	if len(res) <= 0 {
		g.impl.Printf("Py_INCREF(Py_None);\nreturn Py_None;\n")
		return
	}

	format := []string{}
	funcArgs = []string{}
	switch len(res) {
	case 1:
		ret := res[0]
		pyfmt, _ := ret.getArgParse()
		format = append(format, pyfmt)
		funcArgs = append(funcArgs, "c_gopy_ret")
	default:
		for _, ret := range res {
			pyfmt, _ := ret.getArgParse()
			format = append(format, pyfmt)
			funcArgs = append(funcArgs, ret.getFuncArg())
		}
	}

	g.impl.Printf("return Py_BuildValue(%q, %s);\n",
		strings.Join(format, ""),
		strings.Join(funcArgs, ", "),
	)
	//g.impl.Printf("return NULL;\n")
}

func (g *cpyGen) genStruct(cpy Struct) {
	pkgname := cpy.Package().Name()

	//fmt.Printf("obj: %#v\ntyp: %#v\n", obj, typ)
	g.decl.Printf("/* --- decls for struct %s.%v --- */\n", pkgname, cpy.GoName())
	g.decl.Printf("typedef void* GoPy_%s;\n\n", cpy.ID())
	g.decl.Printf("/* type for struct %s.%v\n", pkgname, cpy.GoName())
	g.decl.Printf(" */\ntypedef struct {\n")
	g.decl.Indent()
	g.decl.Printf("PyObject_HEAD\n")
	g.decl.Printf("GoPy_%[1]s cgopy; /* unsafe.Pointer to %[1]s */\n", cpy.ID())
	g.decl.Outdent()
	g.decl.Printf("} _gopy_%s;\n", cpy.ID())
	g.decl.Printf("\n\n")

	g.impl.Printf("/* --- impl for %s.%v */\n\n", pkgname, cpy.GoName())

	g.genStructNew(cpy)
	g.genStructDealloc(cpy)
	g.genStructInit(cpy)
	g.genStructMembers(cpy)
	g.genStructMethods(cpy)

	g.genStructProtocols(cpy)

	g.impl.Printf("static PyTypeObject _gopy_%sType = {\n", cpy.ID())
	g.impl.Indent()
	g.impl.Printf("PyObject_HEAD_INIT(NULL)\n")
	g.impl.Printf("0,\t/*ob_size*/\n")
	g.impl.Printf("\"%s.%s\",\t/*tp_name*/\n", pkgname, cpy.GoName())
	g.impl.Printf("sizeof(_gopy_%s),\t/*tp_basicsize*/\n", cpy.ID())
	g.impl.Printf("0,\t/*tp_itemsize*/\n")
	g.impl.Printf("(destructor)_gopy_%s_dealloc,\t/*tp_dealloc*/\n", cpy.ID())
	g.impl.Printf("0,\t/*tp_print*/\n")
	g.impl.Printf("0,\t/*tp_getattr*/\n")
	g.impl.Printf("0,\t/*tp_setattr*/\n")
	g.impl.Printf("0,\t/*tp_compare*/\n")
	g.impl.Printf("0,\t/*tp_repr*/\n")
	g.impl.Printf("0,\t/*tp_as_number*/\n")
	g.impl.Printf("0,\t/*tp_as_sequence*/\n")
	g.impl.Printf("0,\t/*tp_as_mapping*/\n")
	g.impl.Printf("0,\t/*tp_hash */\n")
	g.impl.Printf("0,\t/*tp_call*/\n")
	g.impl.Printf("_gopy_%s_tp_str,\t/*tp_str*/\n", cpy.ID())
	g.impl.Printf("0,\t/*tp_getattro*/\n")
	g.impl.Printf("0,\t/*tp_setattro*/\n")
	g.impl.Printf("0,\t/*tp_as_buffer*/\n")
	g.impl.Printf("Py_TPFLAGS_DEFAULT,\t/*tp_flags*/\n")
	g.impl.Printf("%q,\t/* tp_doc */\n", cpy.Doc())
	g.impl.Printf("0,\t/* tp_traverse */\n")
	g.impl.Printf("0,\t/* tp_clear */\n")
	g.impl.Printf("0,\t/* tp_richcompare */\n")
	g.impl.Printf("0,\t/* tp_weaklistoffset */\n")
	g.impl.Printf("0,\t/* tp_iter */\n")
	g.impl.Printf("0,\t/* tp_iternext */\n")
	g.impl.Printf("_gopy_%s_methods,             /* tp_methods */\n", cpy.ID())
	g.impl.Printf("0,\t/* tp_members */\n")
	g.impl.Printf("_gopy_%s_getsets,\t/* tp_getset */\n", cpy.ID())
	g.impl.Printf("0,\t/* tp_base */\n")
	g.impl.Printf("0,\t/* tp_dict */\n")
	g.impl.Printf("0,\t/* tp_descr_get */\n")
	g.impl.Printf("0,\t/* tp_descr_set */\n")
	g.impl.Printf("0,\t/* tp_dictoffset */\n")
	g.impl.Printf("(initproc)_gopy_%s_init,      /* tp_init */\n", cpy.ID())
	g.impl.Printf("0,                         /* tp_alloc */\n")
	g.impl.Printf("_gopy_%s_new,\t/* tp_new */\n", cpy.ID())
	g.impl.Outdent()
	g.impl.Printf("};\n\n")

}

func (g *cpyGen) genStructDealloc(cpy Struct) {
	pkgname := cpy.Package().Name()

	g.decl.Printf("/* tp_dealloc for %s.%v */\n", pkgname, cpy.GoName())
	g.decl.Printf("static void\n_gopy_%[1]s_dealloc(_gopy_%[1]s *self);\n",
		cpy.ID(),
	)

	g.impl.Printf("/* tp_dealloc for %s.%v */\n", pkgname, cpy.GoName())
	g.impl.Printf("static void\n_gopy_%[1]s_dealloc(_gopy_%[1]s *self) {\n",
		cpy.ID(),
	)
	g.impl.Indent()
	g.impl.Printf("self->ob_type->tp_free((PyObject*)self);\n")
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genStructNew(cpy Struct) {
	pkgname := cpy.Package().Name()

	g.decl.Printf("/* tp_new for %s.%v */\n", pkgname, cpy.GoName())
	g.decl.Printf(
		"static PyObject*\n_gopy_%s_new(PyTypeObject *type, PyObject *args, PyObject *kwds);\n",
		cpy.ID(),
	)

	g.impl.Printf("/* tp_new */\n")
	g.impl.Printf(
		"static PyObject*\n_gopy_%s_new(PyTypeObject *type, PyObject *args, PyObject *kwds) {\n",
		cpy.ID(),
	)
	g.impl.Indent()
	g.impl.Printf("_gopy_%s *self;\n", cpy.ID())
	g.impl.Printf("self = (_gopy_%s *)type->tp_alloc(type, 0);\n", cpy.ID())
	g.impl.Printf("self->cgopy = GoPy_%s_new();\n", cpy.ID())
	g.impl.Printf("return (PyObject*)self;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genStructInit(cpy Struct) {
	pkgname := cpy.Package().Name()

	g.decl.Printf("/* tp_init for %s.%v */\n", pkgname, cpy.GoName())
	g.decl.Printf(
		"static int\n_gopy_%[1]s_init(_gopy_%[1]s *self, PyObject *args, PyObject *kwds);\n",
		cpy.ID(),
	)

	g.impl.Printf("/* tp_init */\n")
	g.impl.Printf(
		"static int\n_gopy_%[1]s_init(_gopy_%[1]s *self, PyObject *args, PyObject *kwds) {\n",
		cpy.ID(),
	)
	g.impl.Indent()
	g.impl.Printf("return 0;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genStructMembers(cpy Struct) {
	pkgname := cpy.Package().Name()
	typ := cpy.Struct()

	g.decl.Printf("/* tp_getset for %s.%v */\n", pkgname, cpy.GoName())
	for i := 0; i < typ.NumFields(); i++ {
		f := typ.Field(i)
		if !f.Exported() {
			continue
		}
		g.genStructMemberGetter(cpy, i, f)
		g.genStructMemberSetter(cpy, i, f)
	}

	g.impl.Printf("/* tp_getset for %s.%v */\n", pkgname, cpy.GoName())
	g.impl.Printf("static PyGetSetDef _gopy_%s_getsets[] = {\n", cpy.ID())
	g.impl.Indent()
	for i := 0; i < typ.NumFields(); i++ {
		f := typ.Field(i)
		if !f.Exported() {
			continue
		}
		doc := "doc for " + f.Name() // FIXME(sbinet) retrieve doc for fields
		g.impl.Printf("{%q, ", f.Name())
		g.impl.Printf("(getter)_gopy_%[1]s_getter_%[2]d, ", cpy.ID(), i+1)
		g.impl.Printf("(setter)_gopy_%[1]s_setter_%[2]d, ", cpy.ID(), i+1)
		g.impl.Printf("%q, NULL},\n", doc)
	}
	g.impl.Printf("{NULL} /* Sentinel */\n")
	g.impl.Outdent()
	g.impl.Printf("};\n\n")
}

func (g *cpyGen) genStructMemberGetter(cpy Struct, i int, f types.Object) {
	pkg := cpy.Package()
	var (
		recv *Var
		self = newVar(pkg, cpy.GoType(), cpy.GoName(), cpy.GoName(), cpy.Doc())
	)

	ft := f.Type()
	g.decl.Printf("static PyObject*\n")
	g.decl.Printf(
		"_gopy_%[1]s_getter_%[2]d(_gopy_%[1]s *self, void *closure); /* %[3]s */\n",
		cpy.ID(),
		i+1,
		f.Name(),
	)

	g.impl.Printf("static PyObject*\n")
	g.impl.Printf(
		"_gopy_%[1]s_getter_%[2]d(_gopy_%[1]s *self, void *closure) /* %[3]s */ {\n",
		cpy.ID(),
		i+1,
		f.Name(),
	)
	g.impl.Indent()

	var (
		ifield   = newVar(pkg, ft, f.Name(), "ret", "")
		params   = []*Var{self}
		results  = []*Var{ifield}
		fgetname = fmt.Sprintf("GoPy_%[1]s_getter_%[2]d", cpy.ID(), i+1)
		fgetid   = fmt.Sprintf("%[1]s_getter_%[2]d", cpy.ID(), i+1)
		fgetret  = ft
	)

	if false {
		fget := Func{
			pkg:  pkg,
			sig:  newSignature(pkg, recv, params, results),
			typ:  nil,
			name: fgetname,
			id:   fgetid,
			doc:  "",
			ret:  fgetret,
			err:  false,
		}

		g.genFuncBody(fget)
	}

	g.impl.Printf("PyObject *o = NULL;\n")
	ftname := cgoTypeName(ft)
	if needWrapType(ft) {
		ftname = fmt.Sprintf("GoPy_%[1]s_field_%d", cpy.GoName(), i+1)
		g.impl.Printf(
			"%[1]s cgopy_ret = GoPy_%[2]s_getter_%[3]d(self->cgopy);\n",
			ftname,
			cpy.ID(),
			i+1,
		)
	} else if ifield.isGoString() {
		ifield.genDecl(g.impl)
		g.impl.Printf(
			"c_ret = (%[1]s)GoPy_%[2]s_getter_%[3]d(self->cgopy);\n",
			ftname,
			cpy.ID(),
			i+1,
		)
		g.impl.Printf(
			"cgopy_%[1]s = CGoPy_CString(c_%[1]s);\n",
			ifield.Name(),
		)

	} else {
		g.impl.Printf(
			"%[1]s cgopy_ret = GoPy_%[2]s_getter_%[3]d(self->cgopy);\n",
			ftname,
			cpy.ID(),
			i+1,
		)
	}

	{
		format := []string{}
		funcArgs := []string{}
		switch len(results) {
		case 1:
			ret := results[0]
			pyfmt, _ := ret.getArgParse()
			format = append(format, pyfmt)
			funcArgs = append(funcArgs, "cgopy_ret")
		default:
			panic("bind: impossible")
		}
		g.impl.Printf("o = Py_BuildValue(%q, %s);\n",
			strings.Join(format, ""),
			strings.Join(funcArgs, ", "),
		)
	}

	if ifield.isGoString() {
		g.impl.Printf("free((void*)cgopy_%[1]s);\n", ifield.Name())
	}

	g.impl.Printf("return o;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n\n")

}

func (g *cpyGen) genStructMemberSetter(cpy Struct, i int, f types.Object) {
	var (
		pkg      = cpy.Package()
		ft       = f.Type()
		self     = newVar(pkg, cpy.GoType(), cpy.GoName(), "self", "")
		ifield   = newVar(pkg, ft, f.Name(), "ret", "")
		fsetname = fmt.Sprintf("GoPy_%[1]s_setter_%[2]d", cpy.ID(), i+1)
	)

	g.decl.Printf("static int\n")
	g.decl.Printf(
		"_gopy_%[1]s_setter_%[2]d(_gopy_%[1]s *self, PyObject *value, void *closure);\n",
		cpy.ID(),
		i+1,
	)

	g.impl.Printf("static int\n")
	g.impl.Printf(
		"_gopy_%[1]s_setter_%[2]d(_gopy_%[1]s *self, PyObject *value, void *closure) {\n",
		cpy.ID(),
		i+1,
	)
	g.impl.Indent()

	ifield.genDecl(g.impl)
	g.impl.Printf("PyObject *tuple = NULL;\n\n")
	g.impl.Printf("if (value == NULL) {\n")
	g.impl.Indent()
	g.impl.Printf(
		"PyErr_SetString(PyExc_TypeError, \"Cannot delete '%[1]s' attribute\");\n",
		f.Name(),
	)
	g.impl.Printf("return -1;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n")

	// TODO(sbinet) check 'value' type (PyString_Check, PyInt_Check, ...)

	g.impl.Printf("tuple = PyTuple_New(1);\n")
	g.impl.Printf("Py_INCREF(value);\n")
	g.impl.Printf("PyTuple_SET_ITEM(tuple, 0, value);\n\n")

	g.impl.Printf("\nif (!PyArg_ParseTuple(tuple, ")
	pyfmt, pyaddr := ifield.getArgParse()
	g.impl.Printf("%q, %s)) {\n", pyfmt, pyaddr)
	g.impl.Indent()
	g.impl.Printf("Py_DECREF(tuple);\n")
	g.impl.Printf("return -1;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n")
	g.impl.Printf("Py_DECREF(tuple);\n\n")

	if ifield.isGoString() {
		g.impl.Printf(
			"c_%[1]s = CGoPy_GoString((char*)cgopy_%[1]s);\n",
			ifield.Name(),
		)
	}

	g.impl.Printf("%[1]s((%[2]s)(self->cgopy), c_%[3]s);\n",
		fsetname,
		self.CGoType(),
		ifield.Name(),
	)

	g.impl.Printf("return 0;\n")
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genStructMethods(cpy Struct) {

	pkgname := cpy.Package().Name()

	g.decl.Printf("/* methods for %s.%s */\n\n", pkgname, cpy.GoName())
	for _, m := range cpy.meths {
		g.genMethod(cpy, m)
	}

	g.impl.Printf("static PyMethodDef _gopy_%s_methods[] = {\n", cpy.ID())
	g.impl.Indent()
	for _, m := range cpy.meths {
		margs := "METH_VARARGS"
		if m.Return() == nil {
			margs = "METH_NOARGS"
		}
		g.impl.Printf(
			"{%[1]q, (PyCFunction)gopy_%[2]s, %[3]s, %[4]q},\n",
			m.GoName(),
			m.ID(),
			margs,
			m.Doc(),
		)
	}
	g.impl.Printf("{NULL} /* sentinel */\n")
	g.impl.Outdent()
	g.impl.Printf("};\n\n")
}

func (g *cpyGen) genMethod(cpy Struct, fct Func) {
	pkgname := g.pkg.pkg.Name()
	g.decl.Printf("/* wrapper of %[1]s.%[2]s */\n",
		pkgname,
		cpy.GoName()+"."+fct.GoName(),
	)
	g.decl.Printf("static PyObject*\n")
	g.decl.Printf("gopy_%s(PyObject *self, PyObject *args);\n", fct.ID())

	g.impl.Printf("/* wrapper of %[1]s.%[2]s */\n",
		pkgname,
		cpy.GoName()+"."+fct.GoName(),
	)
	g.impl.Printf("static PyObject*\n")
	g.impl.Printf("gopy_%s(PyObject *self, PyObject *args) {\n", fct.ID())
	g.impl.Indent()
	g.genMethodBody(fct)
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genMethodBody(fct Func) {
	g.genFuncBody(fct)
}

func (g *cpyGen) genStructProtocols(cpy Struct) {
	g.genStructTPStr(cpy)
}

func (g *cpyGen) genStructTPStr(cpy Struct) {
	g.decl.Printf("static PyObject*\n_gopy_%s_tp_str(PyObject *self);\n", cpy.ID())

	g.impl.Printf("static PyObject*\n_gopy_%s_tp_str(PyObject *self) {\n",
		cpy.ID(),
	)

	if (cpy.prots & ProtoStringer) == 0 {
		g.impl.Indent()
		g.impl.Printf("return PyObject_Repr(self);\n")
		g.impl.Outdent()
		g.impl.Printf("}\n\n")
		return
	}

	var m Func
	for _, f := range cpy.meths {
		if f.GoName() == "String" {
			m = f
			break
		}
	}

	g.impl.Indent()
	g.impl.Printf("return gopy_%[1]s(self, 0);\n", m.ID())
	g.impl.Outdent()
	g.impl.Printf("}\n\n")
}

func (g *cpyGen) genPreamble() {
	n := g.pkg.pkg.Name()
	g.decl.Printf(cPreamble, n, g.pkg.pkg.Path(), filepath.Base(n))
}
