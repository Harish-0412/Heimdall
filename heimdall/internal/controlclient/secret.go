package controlclient

import (
	"context"
	"encoding/json"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretStore only reads and updates the one operator-created auth Secret.
// The bootstrap key enrollment is replaced by a durable session on exchange.
type SecretStore struct {
	Client          client.Client
	Namespace, Name string
}

func (s *SecretStore) Load(ctx context.Context) (Session, string, error) {
	var secret corev1.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &secret); err != nil {
		return Session{}, "", errors.New("agent auth Secret is unavailable")
	}
	var session Session
	// An operator-provided replacement enrollment intentionally supersedes a
	// revoked/expired session. Exchange still needs a persisted nonce first.
	if b := secret.Data["session.json"]; len(b) > 0 {
		if json.Unmarshal(b, &session) != nil {
			return session, "", errors.New("agent auth session is invalid")
		}
	} else {
		session.Enrollment = string(secret.Data["enrollment"])
	}
	if enrollment := string(secret.Data["enrollment"]); enrollment != "" && enrollment != session.Enrollment {
		session = Session{Enrollment: enrollment}
	}
	return session, secret.ResourceVersion, nil
}
func (s *SecretStore) Save(ctx context.Context, session Session, version string) error {
	var secret corev1.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &secret); err != nil {
		return errors.New("agent auth Secret is unavailable")
	}
	if secret.ResourceVersion != version {
		return errors.New("agent auth session changed")
	}
	b, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data["session.json"] = b
	if session.Enrollment == "" {
		delete(secret.Data, "enrollment")
	}
	if err = s.Client.Update(ctx, &secret); err != nil {
		return errors.New("cannot persist agent session")
	}
	return nil
}
