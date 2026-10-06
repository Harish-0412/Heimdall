package controlclient

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSecretStoreRetainsExchangeNonceAndAcceptsOperatorReenrollment(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "agent", Name: "auth"}, Data: map[string][]byte{"enrollment": []byte("original")}}).Build()
	store := &SecretStore{Client: k, Namespace: "agent", Name: "auth"}
	s, rv, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Nonce = "durable-nonce"
	if err = store.Save(ctx, s, rv); err != nil {
		t.Fatal(err)
	}
	s, _, err = store.Load(ctx)
	if err != nil || s.Nonce != "durable-nonce" {
		t.Fatal("enrollment discarded persisted exchange nonce")
	}
	var secret corev1.Secret
	if k.Get(ctx, types.NamespacedName{Namespace: "agent", Name: "auth"}, &secret) != nil {
		t.Fatal("secret unavailable")
	}
	secret.Data["enrollment"] = []byte("replacement")
	if k.Update(ctx, &secret) != nil {
		t.Fatal("replacement failed")
	}
	s, _, err = store.Load(ctx)
	if err != nil || s.Enrollment != "replacement" || s.Nonce != "" || s.Pair.AccessToken != "" {
		t.Fatal("operator replacement did not clear revoked session")
	}
}
