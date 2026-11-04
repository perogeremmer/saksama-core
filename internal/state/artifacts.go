package state

import "time"

// RecordArtifact records ownership, or refreshes an existing record.
func (s *Session) RecordArtifact(kind, name, engine, note string) {
	for i := range s.Artifacts {
		if s.Artifacts[i].Kind == kind && s.Artifacts[i].Name == name {
			s.Artifacts[i].Note = note
			return
		}
	}
	s.Artifacts = append(s.Artifacts, ArtifactRecord{
		Kind: kind, Name: name, Engine: engine,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Note:      note,
	})
}

// MarkArtifactsSafe allows cleanup to delete them. It is called only when the
// session has ended and nothing is waiting on the recovery boundary.
func (s *Session) MarkArtifactsSafe(note string) {
	for i := range s.Artifacts {
		s.Artifacts[i].SafeToDelete = true
		if note != "" {
			s.Artifacts[i].Note = note
		}
	}
}

// DeletableArtifacts returns the ones cleanup may drop.
func (s *Session) DeletableArtifacts() []ArtifactRecord {
	var out []ArtifactRecord
	for _, a := range s.Artifacts {
		if a.SafeToDelete {
			out = append(out, a)
		}
	}
	return out
}

// ForgetsArtifact drops a record after a successful delete.
func (s *Session) ForgetsArtifact(kind, name string) {
	kept := s.Artifacts[:0]
	for _, a := range s.Artifacts {
		if a.Kind == kind && a.Name == name {
			continue
		}
		kept = append(kept, a)
	}
	s.Artifacts = kept
}
