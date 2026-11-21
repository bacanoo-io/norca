// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"

	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// Functions returns the standard set of functions of Rigger (design 11): those
// of the HCL standard library under the names module authors know from
// other HCL tools. None of them reads a file, the environment or the
// network.
func Functions() map[string]function.Function {
	return map[string]function.Function{
		"abs": stdlib.AbsoluteFunc, "ceil": stdlib.CeilFunc, "floor": stdlib.FloorFunc,
		"log": stdlib.LogFunc, "max": stdlib.MaxFunc, "min": stdlib.MinFunc,
		"parseint": stdlib.ParseIntFunc, "pow": stdlib.PowFunc, "signum": stdlib.SignumFunc,

		"chomp": stdlib.ChompFunc, "format": stdlib.FormatFunc, "formatlist": stdlib.FormatListFunc,
		"indent": stdlib.IndentFunc, "join": stdlib.JoinFunc, "lower": stdlib.LowerFunc,
		"regex": stdlib.RegexFunc, "regexall": stdlib.RegexAllFunc, "regex_replace": stdlib.RegexReplaceFunc,
		"replace": stdlib.ReplaceFunc, "split": stdlib.SplitFunc, "strlen": stdlib.StrlenFunc, "strrev": stdlib.ReverseFunc,
		"substr": stdlib.SubstrFunc, "title": stdlib.TitleFunc, "trim": stdlib.TrimFunc,
		"trimprefix": stdlib.TrimPrefixFunc, "trimsuffix": stdlib.TrimSuffixFunc,
		"trimspace": stdlib.TrimSpaceFunc, "upper": stdlib.UpperFunc,

		"chunklist": stdlib.ChunklistFunc, "coalesce": coalesce, "issensitive": isSensitive,
		"coalescelist": stdlib.CoalesceListFunc, "compact": stdlib.CompactFunc,
		"concat": stdlib.ConcatFunc, "contains": stdlib.ContainsFunc, "distinct": stdlib.DistinctFunc,
		"element": stdlib.ElementFunc, "flatten": stdlib.FlattenFunc, "index": stdlib.IndexFunc,
		"keys": stdlib.KeysFunc, "length": length, "lookup": stdlib.LookupFunc,
		"merge": stdlib.MergeFunc, "range": stdlib.RangeFunc, "reverse": stdlib.ReverseListFunc,
		"setintersection": stdlib.SetIntersectionFunc, "setproduct": stdlib.SetProductFunc,
		"setsubtract": stdlib.SetSubtractFunc, "setunion": stdlib.SetUnionFunc,
		"slice": stdlib.SliceFunc, "sort": stdlib.SortFunc, "values": stdlib.ValuesFunc,
		"zipmap": stdlib.ZipmapFunc,

		"csvdecode": stdlib.CSVDecodeFunc, "jsondecode": stdlib.JSONDecodeFunc,
		"jsonencode": jsonEncode, "yamlencode": yamlEncode, "tomlencode": tomlEncode, "natsconf": natsconf, "formatdate": stdlib.FormatDateFunc,
		"timeadd": stdlib.TimeAddFunc,

		"alltrue": allTrue, "anytrue": anyTrue,
		"can": tryfunc.CanFunc, "try": tryfunc.TryFunc,

		"tobool": toType(cty.Bool), "tonumber": toType(cty.Number), "tostring": toType(cty.String),
		"tolist": toType(cty.List(cty.DynamicPseudoType)), "toset": toType(cty.Set(cty.DynamicPseudoType)),
		"tomap": toType(cty.Map(cty.DynamicPseudoType)),
	}
}

// allTrue is true when every item of a list of booleans is true.
var allTrue = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "list", Type: cty.List(cty.Bool)}},
	Type:   function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		result := cty.True

		for it := args[0].ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !v.IsKnown() {
				return cty.UnknownVal(cty.Bool), nil
			}

			if v.IsNull() {
				return cty.False, nil
			}

			result = result.And(v)
		}

		return result, nil
	},
})

// anyTrue is true when an item of a list of booleans is true.
var anyTrue = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "list", Type: cty.List(cty.Bool)}},
	Type:   function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		result := cty.False

		for it := args[0].ElementIterator(); it.Next(); {
			_, v := it.Element()
			if !v.IsKnown() {
				return cty.UnknownVal(cty.Bool), nil
			}

			if !v.IsNull() {
				result = result.Or(v)
			}
		}

		return result, nil
	},
})

// toType returns a function that converts its argument to ty.
func toType(ty cty.Type) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{
			Name: "v", Type: cty.DynamicPseudoType, AllowNull: true, AllowDynamicType: true, AllowMarked: true,
		}},
		Type: func(args []cty.Value) (cty.Type, error) {
			if !ty.HasDynamicTypes() {
				return ty, nil
			}

			converted, err := convert.Convert(args[0], ty)
			if err != nil {
				return cty.NilType, function.NewArgError(0, err)
			}

			return converted.Type(), nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			converted, err := convert.Convert(args[0], retType)
			if err != nil {
				return cty.NilVal, function.NewArgError(0, err)
			}

			return converted, nil
		},
	})
}

// coalesce is coalesce(...) as module authors know it from other HCL tools:
// the first argument that is neither null nor an empty string.
// stdlib.CoalesceFunc skips nulls only.
var coalesce = function.New(&function.Spec{
	VarParam: &function.Parameter{Name: "vals", Type: cty.DynamicPseudoType, AllowNull: true, AllowUnknown: true},
	Type: func(args []cty.Value) (cty.Type, error) {
		types := make([]cty.Type, len(args))
		for i, a := range args {
			types[i] = a.Type()
		}

		ty, _ := convert.UnifyUnsafe(types)
		if ty == cty.NilType {
			return cty.NilType, errors.New("all arguments must have the same type")
		}

		return ty, nil
	},
	RefineResult: func(b *cty.RefinementBuilder) *cty.RefinementBuilder { return b.NotNull() },
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		for _, a := range args {
			if !a.IsKnown() {
				return cty.UnknownVal(retType), nil
			}

			if a.IsNull() {
				continue
			}

			v, err := convert.Convert(a, retType)
			if err != nil {
				return cty.NilVal, err
			}

			if v.Type() == cty.String && v.AsString() == "" {
				continue
			}

			return v, nil
		}

		return cty.NilVal, errors.New("no argument is neither null nor an empty string")
	},
})

// isSensitive is issensitive(value): whether the value is sensitive.
var isSensitive = function.New(&function.Spec{
	Params: []function.Parameter{{
		Name: attrValue, Type: cty.DynamicPseudoType,
		AllowMarked: true, AllowNull: true, AllowUnknown: true, AllowDynamicType: true,
	}},
	Type: function.StaticReturnType(cty.Bool),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return cty.BoolVal(args[0].HasMark(Sensitive)), nil
	},
})

// length is length(value) as module authors know it from other HCL tools:
// the items of a list, a set, a tuple or a map, the attributes of an
// object, the characters of a string. stdlib.LengthFunc takes collections
// and tuples only.
var length = function.New(&function.Spec{
	Params: []function.Parameter{{Name: attrValue, Type: cty.DynamicPseudoType, AllowDynamicType: true, AllowUnknown: true}},
	Type:   function.StaticReturnType(cty.Number),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		v := args[0]
		ty := v.Type()

		switch {
		case ty == cty.DynamicPseudoType || !v.IsKnown():
			return cty.UnknownVal(cty.Number), nil
		case v.IsNull():
			return cty.NilVal, errors.New("the value is null")
		case ty == cty.String:
			return stdlib.Strlen(v)
		case ty.IsObjectType():
			return cty.NumberIntVal(int64(len(ty.AttributeTypes()))), nil
		case ty.IsCollectionType() || ty.IsTupleType():
			return v.Length(), nil
		default:
			return cty.NilVal, errors.New("the value is not a collection, an object or a string")
		}
	},
})
