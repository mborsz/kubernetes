/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package protobuf

import (
	"fmt"
	"io"

	"github.com/gogo/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
)

func getListMeta(obj runtime.Object) (v1.ListMeta, error) {
	v, err := conversion.EnforcePtr(obj)
	if err != nil {
		return v1.ListMeta{}, err
	}
	listMeta := v.FieldByName("ListMeta")
	if !listMeta.IsValid() {
		return v1.ListMeta{}, fmt.Errorf("expected ListMeta")
	}
	return listMeta.Interface().(v1.ListMeta), nil
}

// MarshalToWriter marshals object referenceList
func MarshalToWriter(refObj runtime.Object, items <-chan runtime.Object, w io.Writer) error {
	if items, err := meta.ExtractList(refObj); err != nil {
		return fmt.Errorf("failed to extract items: %v", err)
	} else if len(items) != 0 {
		return fmt.Errorf("got obj with nonzero items: %v", len(items))
	}
	if _, err := w.Write([]byte{0xa}); err != nil {
		return err
	}
	listMeta, err := getListMeta(refObj)
	if err != nil {
		return fmt.Errorf("failed to get listMeta: %v", err)
	}
	data, err := listMeta.Marshal()
	if err != nil {
		return err
	}
	if err := writeVarintGenerated(w, uint64(len(data))); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}

	for item := range items {
		marshaler, ok := item.(proto.Marshaler)
		if !ok {
			return fmt.Errorf("item doesn't implement proto.Marshaler: %v", item)
		}
		data, err := marshaler.Marshal()
		if err != nil {
			return err
		}

		// TODO(mborsz): Make 0x12 depend on the actual proto tag of items.
		if _, err := w.Write([]byte{0x12}); err != nil {
			return err
		}

		if err := writeVarintGenerated(w, uint64(len(data))); err != nil {
			return err
		}

		if _, err := w.Write(data); err != nil {
			return err
		}
	}

	return nil
}

func writeVarintGenerated(w io.Writer, v uint64) error {
	for v >= 1<<7 {
		if _, err := w.Write([]byte{uint8(v&0x7f | 0x80)}); err != nil {
			return err
		}
		v >>= 7
	}
	_, err := w.Write([]byte{uint8(v)})
	return err
}
