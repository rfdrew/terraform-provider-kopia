package kopiaclient

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Kopia's PUT replaces a policy as a whole and keeps nothing of the previous
// one (internal/server/api_policies.go, handlePolicyPut), while Policy models
// only the fields of the releases this package knows. A later release can add
// fields, which a user may set in the Kopia UI or with `kopia policy set`, and
// a body built from a Policy alone would silently erase them. A write
// therefore goes over the policy the target stores: every field Policy models
// is taken from the Policy, set or, when the Policy leaves it out, removed,
// and every other field is sent back exactly as Kopia reported it, at every
// level of the policy.
//
// Kopia decodes a policy with plain json.Unmarshal, which drops the keys it
// does not know and matches the others to its fields case-insensitively, with
// the folding strings.EqualFold applies. A stored key that matches a modelled
// one in any case is therefore that field, and belongs to the Policy; kept
// beside it, it would set the field the Policy removes.

// policyObject is a JSON object of a policy as Policy models it: the keys it
// has, in the order Policy encodes them.
type policyObject struct {
	keys []policyKey
}

// policyKey is a key Policy models.
type policyKey struct {
	name string
	// goType is the type of the field, which a stored value is decoded into
	// to compare it with the Policy's.
	goType reflect.Type
	// object describes the object the key holds; nil for any other value, a
	// number, a string or a list, which belongs to the Policy as a whole,
	// apart from what elements keeps.
	object *policyObject
	// elements describes the elements of a list of objects, such as the
	// times of day, whose stored elements keep their unmodelled fields when
	// the Policy has an element of the same value.
	elements *policyObject
	// sameWhole is set for an object Kopia has as a pointer, an action, which
	// is there or not as a whole. An action the Policy leaves out is removed
	// together with any field of it Policy does not model, which kept on its
	// own would leave Kopia an action without a command, failing every
	// snapshot it runs before. An action the Policy has keeps those fields
	// only when sameWhole says that the stored action is the same one, as a
	// field a later release adds may well belong to the command it was set
	// with. Every other object is a plain struct in Kopia, for which an empty
	// object means what an absent one does.
	sameWhole func(stored, desired json.RawMessage) bool
}

// policyWholeTypes are the types Kopia has as pointers, with what makes a
// stored value the same as the Policy's.
var policyWholeTypes = map[reflect.Type]func(stored, desired json.RawMessage) bool{
	reflect.TypeOf(ActionCommand{}): policySameAction,
}

// policySameAction reports whether two actions run the same command: the same
// path with the same arguments, and the same script. The timeout and the mode
// only say how it runs.
func policySameAction(stored, desired json.RawMessage) bool {
	var s, d ActionCommand
	if json.Unmarshal(stored, &s) != nil || json.Unmarshal(desired, &d) != nil {
		return false
	}
	return s.Command == d.Command && s.Script == d.Script && slices.Equal(s.Arguments, d.Arguments)
}

// policyModel is the object of a whole policy. It is derived from the fields
// of Policy as encoding/json sees them, so what the merge takes as modelled
// is what Policy encodes.
var policyModel = policyObjectOf(reflect.TypeOf(policyWire{}))

var (
	policyJSONMarshaler   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	policyJSONUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	policyTextMarshaler   = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// policyEncodesAsObject reports whether encoding/json encodes a value of
// struct type t as an object of its fields, rather than as whatever its own
// methods produce.
func policyEncodesAsObject(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	for _, methods := range []reflect.Type{policyJSONMarshaler, policyJSONUnmarshaler, policyTextMarshaler} {
		if t.Implements(methods) || reflect.PointerTo(t).Implements(methods) {
			return false
		}
	}
	return true
}

// policyObjectOf describes the JSON object of struct type t. It panics on an
// embedded field, whose fields encoding/json would move into t's object,
// which the merge does not follow; no policy type has one.
func policyObjectOf(t reflect.Type) *policyObject {
	object := &policyObject{}
	for i := range t.NumField() {
		field := t.Field(i)
		if field.Anonymous {
			panic(fmt.Sprintf("kopiaclient: %s embeds %s, which the policy merge does not support", t, field.Type))
		}
		tag := field.Tag.Get("json")
		if !field.IsExported() || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		key := policyKey{name: name, goType: field.Type}
		valueType := field.Type
		if valueType.Kind() == reflect.Pointer {
			valueType = valueType.Elem()
		}
		switch {
		case policyEncodesAsObject(valueType):
			key.object = policyObjectOf(valueType)
			key.sameWhole = policyWholeTypes[valueType]
		case valueType.Kind() == reflect.Slice && policyEncodesAsObject(valueType.Elem()):
			key.elements = policyObjectOf(valueType.Elem())
		}
		object.keys = append(object.keys, key)
	}
	return object
}

// models reports whether name is one of the object's keys, in any case.
func (o *policyObject) models(name string) bool {
	for _, key := range o.keys {
		if strings.EqualFold(key.name, name) {
			return true
		}
	}
	return false
}

// policyMember is a member of a JSON object, with its value as it was sent.
type policyMember struct {
	name  string
	value json.RawMessage
}

// policyMembers splits data, which must be valid JSON, into the members of
// the object it holds, in their order and with any duplicates; isObject is
// false, with no members, when it holds something else, such as null.
func policyMembers(data json.RawMessage) (members []policyMember, isObject bool, err error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false, err
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false, err
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, err
		}
		members = append(members, policyMember{name: name, value: value})
	}
	return members, true, nil
}

// policyElements splits data, which must be valid JSON, into the elements of
// the list it holds; none when it holds something else.
func policyElements(data json.RawMessage) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, err
	}
	var elements []json.RawMessage
	for decoder.More() {
		var element json.RawMessage
		if err := decoder.Decode(&element); err != nil {
			return nil, err
		}
		elements = append(elements, element)
	}
	return elements, nil
}

// policyMemberValue returns the value of name in members, which hold an
// object Policy encoded, with every key once and in Policy's spelling.
func policyMemberValue(members []policyMember, name string) (json.RawMessage, bool) {
	for _, member := range members {
		if member.name == name {
			return member.value, true
		}
	}
	return nil, false
}

// policyObjectJSON encodes members as a JSON object, in their order.
func policyObjectJSON(members []policyMember) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, member := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		// Encoding a string cannot fail.
		name, _ := json.Marshal(member.name)
		b.Write(name)
		b.WriteByte(':')
		b.Write(member.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// policyListJSON encodes elements as a JSON list.
func policyListJSON(elements []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, element := range elements {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(element)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// policyDecodesEqual reports whether a and b decode to the same value of type
// t, as Kopia would see them.
func policyDecodesEqual(t reflect.Type, a, b json.RawMessage) bool {
	va, vb := reflect.New(t), reflect.New(t)
	if json.Unmarshal(a, va.Interface()) != nil || json.Unmarshal(b, vb.Interface()) != nil {
		return false
	}
	return reflect.DeepEqual(va.Elem().Interface(), vb.Elem().Interface())
}

// merge returns the members of one object of the body: desired holds the
// object as Policy encodes it, stored the members of the same object in the
// policy Kopia stores. The keys o models come first, in Policy's order and
// with Policy's values; then every member of stored that matches none of
// them, as it was sent and in its order.
func (o *policyObject) merge(stored, desired []policyMember) ([]policyMember, error) {
	var merged []policyMember
	for _, key := range o.keys {
		value, set := policyMemberValue(desired, key.name)
		// Kopia decodes every stored key matching this one into the same
		// field, so all of them are this key's stored values.
		var storedValues []json.RawMessage
		for _, member := range stored {
			if strings.EqualFold(member.name, key.name) {
				storedValues = append(storedValues, member.value)
			}
		}

		switch {
		case key.object == nil && !set:
		case key.object == nil && key.elements != nil && len(storedValues) > 0:
			// Kopia's answers never repeat a key. Should a stored policy
			// do, Go would decode each later list into the elements of the
			// one before; only the last one is compared here, which can
			// drop unmodelled fields of such a policy, never add any.
			list, err := key.elements.mergeList(key.goType.Elem(), storedValues[len(storedValues)-1], value)
			if err != nil {
				return nil, err
			}
			merged = append(merged, policyMember{name: key.name, value: list})
		case key.object == nil:
			merged = append(merged, policyMember{name: key.name, value: value})
		case key.sameWhole != nil && !set:
		default:
			// The unmodelled fields of every stored object are kept, as
			// Kopia decodes them all into the same struct; a value that is
			// not an object has none. A null does nothing to a struct, but
			// unsets a pointer, so a whole object starts again after one.
			var storedInner []policyMember
			for _, storedValue := range storedValues {
				if key.sameWhole != nil && policyIsNull(storedValue) {
					storedInner = nil
					continue
				}
				inner, _, err := policyMembers(storedValue)
				if err != nil {
					return nil, err
				}
				storedInner = append(storedInner, inner...)
			}
			// A whole object the Policy replaces with another one keeps
			// none of them.
			if key.sameWhole != nil && !key.sameWhole(policyObjectJSON(storedInner), value) {
				storedInner = nil
			}
			var desiredInner []policyMember
			if set {
				inner, _, err := policyMembers(value)
				if err != nil {
					return nil, err
				}
				desiredInner = inner
			}
			inner, err := key.object.merge(storedInner, desiredInner)
			if err != nil {
				return nil, err
			}
			// An object the Policy leaves out is sent only for the fields of
			// it that are kept, so that an empty section is never sent; an
			// empty action the Policy has is sent as it is.
			if set || len(inner) > 0 {
				merged = append(merged, policyMember{name: key.name, value: policyObjectJSON(inner)})
			}
		}
	}

	// Whatever Kopia reported is sent back as it is, even null, {}, [], false
	// or 0: Kopia decodes the same value from it as from what it stores, which
	// leaving it out or tidying it up would not always do. A {} of a field
	// that is a pointer in Kopia, for one, is not the same as its absence.
	for _, member := range stored {
		if !o.models(member.name) {
			merged = append(merged, member)
		}
	}
	return merged, nil
}

// mergeList returns the list desired, the elements of type t the Policy has,
// with the unmodelled fields of an element of stored that has the same value
// kept in each element. Each stored element stands for one element of desired
// at most.
func (o *policyObject) mergeList(t reflect.Type, stored, desired json.RawMessage) (json.RawMessage, error) {
	storedElements, err := policyElements(stored)
	if err != nil {
		return nil, err
	}
	desiredElements, err := policyElements(desired)
	if err != nil {
		return nil, err
	}
	used := make([]bool, len(storedElements))
	for i, element := range desiredElements {
		for j, storedElement := range storedElements {
			if used[j] || !policyDecodesEqual(t, storedElement, element) {
				continue
			}
			used[j] = true
			storedMembers, _, err := policyMembers(storedElement)
			if err != nil {
				return nil, err
			}
			desiredMembers, _, err := policyMembers(element)
			if err != nil {
				return nil, err
			}
			members, err := o.merge(storedMembers, desiredMembers)
			if err != nil {
				return nil, err
			}
			desiredElements[i] = policyObjectJSON(members)
			break
		}
	}
	return policyListJSON(desiredElements), nil
}

// mergePolicyJSON returns the body of a PUT that writes policy over stored,
// the policy the target stores now, as GetPolicyJSON returned it: every field
// Policy models is taken from policy, which removes it when policy leaves it
// out, and every other field of stored, at any level, is kept as Kopia sent
// it, apart from its whitespace. nil, empty or null stored stands for a
// target without a policy, and nil policy for the empty policy.
//
// The fields of policy are sent as Policy encodes them and in its order, and
// the kept ones after them, so without any the body is exactly the encoding
// of policy. An object policy leaves out is sent only when fields of it are
// kept, so policy's rules hold for everything it models: no empty section, no
// empty list, no plain false or 0.
func mergePolicyJSON(stored json.RawMessage, policy *Policy) (json.RawMessage, error) {
	if policy == nil {
		policy = &Policy{}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	desired, _, err := policyMembers(encoded)
	if err != nil {
		return nil, err
	}

	var kept []policyMember
	if len(bytes.TrimSpace(stored)) > 0 {
		if !json.Valid(stored) {
			return nil, errors.New("the stored policy is not valid JSON")
		}
		members, isObject, err := policyMembers(stored)
		if err != nil {
			return nil, err
		}
		if !isObject && !policyIsNull(stored) {
			return nil, errors.New("the stored policy is not a JSON object")
		}
		kept = members
	}

	merged, err := policyModel.merge(kept, desired)
	if err != nil {
		return nil, err
	}
	// Marshalling compacts the kept values, which Kopia sends indented.
	return json.Marshal(policyObjectJSON(merged))
}

// policyIsNull reports whether data is the JSON null.
func policyIsNull(data json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// KeptPolicyFields lists the fields a write of policy over stored keeps
// because Policy does not model them, as dotted paths such as
// "scheduling.newField", in the order they are sent; a field of the
// elements of a list, such as the times of day, is listed once under the
// list. It lists none when there are none, or when the merge fails.
func KeptPolicyFields(stored json.RawMessage, policy *Policy) []string {
	merged, err := mergePolicyJSON(stored, policy)
	if err != nil {
		return nil
	}
	var fields []string
	policyModel.unmodelled(merged, "", &fields)
	return fields
}

// unmodelled adds the paths of the members of data, an object of o, that o
// does not model, at any depth, to fields, each once.
func (o *policyObject) unmodelled(data json.RawMessage, prefix string, fields *[]string) {
	members, _, _ := policyMembers(data)
	for _, member := range members {
		path := prefix + member.name
		var key *policyKey
		for i := range o.keys {
			if strings.EqualFold(o.keys[i].name, member.name) {
				key = &o.keys[i]
			}
		}
		switch {
		case key == nil:
			if !slices.Contains(*fields, path) {
				*fields = append(*fields, path)
			}
		case key.object != nil:
			key.object.unmodelled(member.value, path+".", fields)
		case key.elements != nil:
			elements, _ := policyElements(member.value)
			for _, element := range elements {
				key.elements.unmodelled(element, path+".", fields)
			}
		}
	}
}
