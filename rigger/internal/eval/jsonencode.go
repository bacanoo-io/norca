// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// jsonEncode is jsonencode(value): the value as JSON on one line, with the
// attributes of an object by name and null kept.
var jsonEncode = encoder(jsonValue)

func jsonValue(v cty.Value) (string, error) {
	switch {
	case v.IsNull():
		return nullText, nil
	case isObject(v):
		keys, m := sortedKeys(v)
		parts := make([]string, 0, len(keys))

		for _, k := range keys {
			s, err := jsonValue(m[k])
			if err != nil {
				return "", fmt.Errorf("%s: %w", k, err)
			}

			parts = append(parts, `"`+jsonFormat.escape(k)+`":`+s)
		}

		return "{" + strings.Join(parts, ",") + "}", nil
	case isList(v):
		items, services, err := listItems(v, true)
		if err != nil {
			return "", err
		}

		return inlineList(items, services, ",", jsonValue)
	default:
		return jsonFormat.scalar(v)
	}
}
