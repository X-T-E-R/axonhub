package objects

func (p *APIKeyProfiles) Clone() *APIKeyProfiles {
	if p == nil {
		return nil
	}
	out := &APIKeyProfiles{ActiveProfile: p.ActiveProfile, Profiles: make([]APIKeyProfile, len(p.Profiles))}
	for i := range p.Profiles {
		out.Profiles[i] = *p.Profiles[i].Clone()
	}
	return out
}
