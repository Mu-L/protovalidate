// Copyright 2023-2025 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package migrator

import (
	"bytes"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protovalidate/tools/internal/gen/buf/validate"
)

type FieldVisitor struct {
	PrinterVisitor
}

func (v *FieldVisitor) VisitCompactOptionsNode(node *ast.CompactOptionsNode) error {
	buf := &bytes.Buffer{}
	prevW := v.w
	v.w = buf
	defer func() { v.w = prevW }()

	hasValues := false
	children := node.Children()
	var lastSep *ast.RuneNode

	for i, child := range children {
		switch childNode := child.(type) {
		case *ast.OptionNode:
			var prevComma *ast.RuneNode
			if hasValues {
				prevComma = lastSep
			}
			nextComma := children[i+1]
			ok, err := v.HandleOption(childNode, prevComma, nextComma)
			if err != nil {
				return err
			}
			hasValues = hasValues || ok
		case *ast.RuneNode:
			if childNode.Rune == ',' {
				lastSep = childNode
				continue
			}
			if err := v.PrintNodes(true, childNode); err != nil {
				return err
			}
		default:
			if err := v.PrintNodes(true, childNode); err != nil {
				return err
			}
		}
	}
	if hasValues {
		if _, err := prevW.Write(buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (v *FieldVisitor) HandleOption(
	node *ast.OptionNode,
	prevComma *ast.RuneNode,
	nextComma ast.Node,
) (wrote bool, err error) {
	name := node.Name.Parts[0]
	isPGV := name.Value() == "(validate.rules)"
	isPV := name.Value() == "(buf.validate.field)"
	needsComma := prevComma != nil

	if v.ShouldPrintOriginal(isPGV, isPV) {
		if needsComma {
			if err = v.printComma(true, prevComma); err != nil {
				return wrote, err
			}
		}
		if err = v.PrintNodes(true, node); err != nil {
			return wrote, err
		}
		wrote = true
		needsComma = true
	}

	if !v.ShouldPrintReplacement(isPGV) {
		return wrote, nil
	}

	nameParts, val, done := v.buildNameParts(node)
	if done {
		return wrote, nil
	}
	wrote = true

	if needsComma {
		if err = v.printComma(false, nextComma, prevComma); err != nil {
			return wrote, err
		}
	}

	if err = v.PrintNodes(!needsComma, append(nameParts, node.Equals)...); err != nil {
		return wrote, err
	}

	if valMsg, ok := val.(*ast.MessageLiteralNode); ok {
		return wrote, HandleMessageLiteral(
			v.PrinterVisitor,
			v.file.NodeInfo(nameParts[len(nameParts)-1]).RawText(),
			node.Name,
			node.Equals,
			true,
			valMsg,
		)
	}

	return wrote, v.PrintNodes(!needsComma, val)
}

func (v *FieldVisitor) printComma(printComments bool, commas ...ast.Node) error {
	for _, comma := range commas {
		if c, ok := comma.(*ast.RuneNode); ok && c != nil && c.Rune == ',' {
			return v.PrintNodes(printComments, c)
		}
	}
	_, err := v.w.Write([]byte(","))
	return err
}

func (v *FieldVisitor) buildNameParts(node *ast.OptionNode) (nameParts []ast.Node, value ast.Node, done bool) {
	value = node.Val
	nameParts = make([]ast.Node, 1, len(node.Name.Parts)*2+1)
	nameParts[0] = v.replaceNode(node.Name.Parts[0], "(buf.validate.field)")
	for i := range len(node.Name.Dots) {
		dot := node.Name.Dots[i]
		part := node.Name.Parts[i+1]
		switch part.Value() {
		case "no_sparse":
			// no_sparse has been removed
			return nil, nil, true
		case "message":
			// ignore message element
			continue
		case "ignore_empty":
			// moved up & renamed, drop the parent element from the path
			nameParts = append(nameParts[:len(nameParts)-2], dot, v.replaceNode(part, "ignore"))
			value = v.replaceNode(value, validate.Ignore_IGNORE_IF_ZERO_VALUE.String())
		case "required":
			switch node.Name.Parts[i].Value() {
			case "any", "timestamp", "duration":
				// removed, drop the WKT from the path
				nameParts = nameParts[:len(nameParts)-2]
			}
			nameParts = append(nameParts, dot, part)
		case "skip":
			// merged with ignore
			nameParts = append(nameParts, dot, v.replaceNode(part, "ignore"))
			value = v.replaceNode(value, validate.Ignore_IGNORE_ALWAYS.String())
		default:
			nameParts = append(nameParts, dot, part)
		}
	}
	return nameParts, value, false
}

type MessageLiteralVisitor struct {
	parent         string
	wroteItem      bool
	requiredNeeded ast.ValueNode
	ignoreNeeded   ast.Node
	PrinterVisitor
}

func HandleMessageLiteral(
	printer PrinterVisitor,
	parent string,
	name, equals ast.Node,
	prefixed bool,
	valMsg *ast.MessageLiteralNode,
) error {
	msgLitVisitor := &MessageLiteralVisitor{
		parent:         parent,
		PrinterVisitor: printer,
	}
	if err := ast.VisitChildren(valMsg, msgLitVisitor); err != nil {
		return err
	}

	if msgLitVisitor.requiredNeeded != nil {
		nodeName := ", required"
		if prefixed {
			nodeName = ", (buf.validate.field).required"
		}

		if err := printer.PrintNodes(false,
			printer.replaceNode(name, nodeName),
			equals,
			msgLitVisitor.requiredNeeded,
		); err != nil {
			return err
		}
	}

	if msgLitVisitor.ignoreNeeded != nil {
		newName := ", ignore"
		if prefixed {
			newName = ", (buf.validate.field).ignore"
		}
		name = printer.replaceNode(name, newName)

		if err := printer.PrintNodes(false,
			name,
			equals,
			msgLitVisitor.ignoreNeeded,
		); err != nil {
			return err
		}
	}

	return nil
}

func (v *MessageLiteralVisitor) VisitMessageFieldNode(node *ast.MessageFieldNode) error {
	v.wroteItem = true
	var (
		name  ast.Node = node.Name
		value ast.Node = node.Val
	)
	switch node.Name.Value() {
	case "message":
		// flatten message field into parent
		if msg, ok := node.Val.(*ast.MessageLiteralNode); ok {
			return ast.VisitChildren(&unwrappedMessageLiteral{MessageLiteralNode: msg}, v)
		}
		v.wroteItem = false
		return nil
	case "skip":
		if val, ok := node.Val.Value().(ast.Identifier); ok && val != "true" {
			v.wroteItem = false
			return nil
		}
		value = v.replaceNode(node.Val, validate.Ignore_IGNORE_ALWAYS.String())
		if v.parent != "items" && v.parent != "keys" && v.parent != "values" {
			// need to lift if it's not an element rule
			v.wroteItem = false
			v.ignoreNeeded = value
			return nil
		}
		name = v.replaceNode(name, "ignore")
	case "ignore_empty":
		if val, ok := node.Val.Value().(ast.Identifier); ok && val == "true" {
			v.ignoreNeeded = v.replaceNode(node.Val, validate.Ignore_IGNORE_IF_ZERO_VALUE.String())
		}
		v.wroteItem = false
		return nil
	case "required":
		if v.parent == "any" || v.parent == "timestamp" || v.parent == "duration" {
			v.requiredNeeded = node.Val
			v.wroteItem = false
			return nil
		}
		return v.PrintNodes(false, node)
	case "no_sparse":
		v.wroteItem = false
		return nil
	}

	nodes := []ast.Node{name}
	if node.Sep != nil {
		nodes = append(nodes, node.Sep)
	}
	if err := v.PrintNodes(false, nodes...); err != nil {
		return err
	}

	if msg, ok := value.(*ast.MessageLiteralNode); ok {
		return HandleMessageLiteral(
			v.PrinterVisitor,
			node.Name.Value(),
			node.Name,
			node.Sep,
			false,
			msg,
		)
	}

	return v.PrintNodes(false, value)
}

func (v *MessageLiteralVisitor) VisitRuneNode(node *ast.RuneNode) error {
	switch node.Rune {
	case ',', ';':
		if !v.wroteItem {
			return nil
		}
		fallthrough
	default:
		v.wroteItem = false
		return v.PrintNodes(true, node)
	}
}
