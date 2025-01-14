package expr

import (
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func (qe *QueryExpr) QuerySQL() string {
	return qe.sql
}

func (qe *QueryExpr) QueryArgs() []any {
	return qe.args
}

func (qe *QueryExpr) HasOutputs() bool {
	return len(qe.outputs) > 0
}

type QueryExpr struct {
	sql     string
	args    []any
	outputs []typeMember
}

// Query returns a query expression ready for execution, using the provided values to
// substitute the input placeholders in the prepared expression. These placeholders use
// the syntax "$Person.fullname", where Person would be a type such as:
//
//	type Person struct {
//	        Name string `db:"fullname"`
//	}
func (pe *PreparedExpr) Query(args ...any) (ce *QueryExpr, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("invalid input parameter: %s", err)
		}
	}()

	var inQuery = make(map[reflect.Type]bool)
	for _, typeMember := range pe.inputs {
		inQuery[typeMember.outerType()] = true
	}

	var typeValue = make(map[reflect.Type]reflect.Value)
	var typeNames []string
	for _, arg := range args {
		v := reflect.ValueOf(arg)
		if v.Kind() == reflect.Invalid || (v.Kind() == reflect.Pointer && v.IsNil()) {
			return nil, fmt.Errorf("need struct or map, got nil")
		}
		v = reflect.Indirect(v)
		t := v.Type()
		if v.Kind() != reflect.Struct && v.Kind() != reflect.Map {
			return nil, fmt.Errorf("need struct or map, got %s", t.Kind())
		}
		if _, ok := typeValue[t]; ok {
			return nil, fmt.Errorf("type %q provided more than once", t.Name())
		}
		typeValue[t] = v
		typeNames = append(typeNames, t.Name())
		if !inQuery[t] {
			// Check if we have a type with the same name from a different package.
			for _, typeMember := range pe.inputs {
				if t.Name() == typeMember.outerType().Name() {
					return nil, fmt.Errorf("type %s not passed as a parameter, have %s", typeMember.outerType().String(), t.String())
				}
			}
			return nil, fmt.Errorf("%s not referenced in query", t.Name())
		}
	}

	// Query parameteres.
	qargs := []any{}
	for i, typeMember := range pe.inputs {
		outerType := typeMember.outerType()
		v, ok := typeValue[outerType]
		if !ok {
			if len(typeNames) == 0 {
				return nil, fmt.Errorf(`type %q not passed as a parameter`, outerType.Name())
			} else {
				return nil, fmt.Errorf(`type %q not passed as a parameter, have: %s`, outerType.Name(), strings.Join(typeNames, ", "))
			}
		}
		var val reflect.Value
		switch tm := typeMember.(type) {
		case *structField:
			val = v.Field(tm.index)
		case *mapKey:
			val = v.MapIndex(reflect.ValueOf(tm.name))
			if val.Kind() == reflect.Invalid {
				return nil, fmt.Errorf(`map %q does not contain key %q`, outerType.Name(), tm.name)
			}
		}
		qargs = append(qargs, sql.Named("sqlair_"+strconv.Itoa(i), val.Interface()))
	}
	return &QueryExpr{outputs: pe.outputs, sql: pe.sql, args: qargs}, nil
}

// ScanArgs returns list of pointers to the struct fields that are listed in qe.outputs.
// All the structs and maps mentioned in the query must be in outputArgs.
func (qe *QueryExpr) ScanArgs(columns []string, outputArgs []any) (scanArgs []any, onSuccess func(), err error) {
	var typesInQuery = []string{}
	var inQuery = make(map[reflect.Type]bool)
	for _, typeMember := range qe.outputs {
		outerType := typeMember.outerType()
		if ok := inQuery[outerType]; !ok {
			inQuery[outerType] = true
			typesInQuery = append(typesInQuery, outerType.Name())
		}
	}

	type mapSetInfo struct {
		keyVal  reflect.Value
		scanVal reflect.Value
		mapVal  reflect.Value
	}
	var mapSetInfos = []mapSetInfo{}
	var typeDest = make(map[reflect.Type]reflect.Value)
	outputVals := []reflect.Value{}
	for _, outputArg := range outputArgs {
		if outputArg == nil {
			return nil, nil, fmt.Errorf("need map or pointer to struct, got nil")
		}
		outputVal := reflect.ValueOf(outputArg)
		k := outputVal.Kind()
		if k != reflect.Map {
			if k != reflect.Pointer {
				return nil, nil, fmt.Errorf("need map or pointer to struct, got %s", k)
			}
			if outputVal.IsNil() {
				return nil, nil, fmt.Errorf("got nil pointer")
			}
			outputVal = outputVal.Elem()
			k = outputVal.Kind()
			if k != reflect.Struct && k != reflect.Map {
				return nil, nil, fmt.Errorf("need map or pointer to struct, got pointer to %s", k)
			}
		}
		if !inQuery[outputVal.Type()] {
			return nil, nil, fmt.Errorf("type %q does not appear in query, have: %s", outputVal.Type().Name(), strings.Join(typesInQuery, ", "))
		}
		if _, ok := typeDest[outputVal.Type()]; ok {
			return nil, nil, fmt.Errorf("type %q provided more than once, rename one of them", outputVal.Type().Name())
		}
		typeDest[outputVal.Type()] = outputVal
		outputVals = append(outputVals, outputVal)
	}

	// Generate the pointers.
	var ptrs = []any{}
	for _, column := range columns {
		idx, ok := markerIndex(column)
		if !ok {
			// Columns not mentioned in output expressions are scanned into x.
			var x any
			ptrs = append(ptrs, &x)
			continue
		}
		if idx >= len(qe.outputs) {
			return nil, nil, fmt.Errorf("internal error: sqlair column not in outputs (%d>=%d)", idx, len(qe.outputs))
		}
		typeMember := qe.outputs[idx]
		outputVal, ok := typeDest[typeMember.outerType()]
		if !ok {
			return nil, nil, fmt.Errorf("type %q found in query but not passed to get", typeMember.outerType().Name())
		}
		switch tm := typeMember.(type) {
		case *structField:
			val := outputVal.Field(tm.index)
			if !val.CanSet() {
				return nil, nil, fmt.Errorf("internal error: cannot set field %s of struct %s", tm.name, tm.structType.Name())
			}
			ptrs = append(ptrs, val.Addr().Interface())
		case *mapKey:
			scanVal := reflect.New(tm.mapType.Elem()).Elem()
			ptrs = append(ptrs, scanVal.Addr().Interface())
			mapSetInfos = append(mapSetInfos, mapSetInfo{keyVal: reflect.ValueOf(tm.name), scanVal: scanVal, mapVal: outputVal})
		}
	}

	onSuccess = func() {
		for _, mi := range mapSetInfos {
			mi.mapVal.SetMapIndex(mi.keyVal, mi.scanVal)
		}
	}

	return ptrs, onSuccess, nil
}
