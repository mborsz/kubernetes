package protobuf

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/gogo/protobuf/proto"
	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestSimplePodListStreaming(t *testing.T) {
	a := &corev1.PodList{
		ListMeta: v1.ListMeta{
			ResourceVersion:    "123456",
			SelfLink:           "self/link",
			Continue:           "some-base64-stuff",
			RemainingItemCount: proto.Int64(1234),
		},
		Items: []corev1.Pod{
			{ObjectMeta: v1.ObjectMeta{Name: "pod-1"}},
			{ObjectMeta: v1.ObjectMeta{Name: "pod-2"}},
			{ObjectMeta: v1.ObjectMeta{Name: "pod-3"}},
		},
	}

	ch := make(chan runtime.Object, len(a.Items))
	for _, pod := range a.Items {
		pod := pod
		ch <- &pod
	}
	close(ch)
	refObj := &corev1.PodList{
		ListMeta: a.ListMeta,
	}
	data, err := a.Marshal()
	require.NoError(t, err, "unexpected error while serializing PodList")
	buf := &bytes.Buffer{}
	err = MarshalToWriter(refObj, ch, buf)
	require.NoError(t, err, "unexpected error while serializing PodListStreaming")

	assert.Equal(t, data, buf.Bytes())
}

func TestFuzzPodListStreaming(t *testing.T) {
	f := fuzz.New()
	for i := 0; i < 100; i++ {
		t.Run(fmt.Sprintf("Run %d/100", i), func(t *testing.T) {
			a := &corev1.PodList{}
			f.Fuzz(a)

			t.Logf("PodList: %+v", a)

			ch := make(chan runtime.Object, len(a.Items))
			for _, pod := range a.Items {
				pod := pod
				ch <- &pod
			}
			close(ch)
			refObj := &corev1.PodList{
				ListMeta: a.ListMeta,
			}
			data, err := a.Marshal()
			require.NoError(t, err, "unexpected error while serializing PodList")
			buf := &bytes.Buffer{}
			err = MarshalToWriter(refObj, ch, buf)
			require.NoError(t, err, "unexpected error while serializing PodListStreaming")
			assert.Equal(t, data, buf.Bytes())

		})
	}
}
