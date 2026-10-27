package core

import "testing"

func TestHasPolicyEntries(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name     string
		list     string
		expected bool
	}{
		{"empty", "", false},
		{
			"without policies",
			";\n; Archive created at 2026-10-26\n3; 3079 16386 EXTENSION - vector \n215; 1259 16400 TABLE public posts app\n",
			false,
		},
		{
			"policy name in another entry",
			"215; 1259 16400 TABLE public POLICY app\n",
			false,
		},
		{
			"with policies",
			"215; 1259 16400 TABLE public posts app\n5012; 3256 17201 POLICY public posts rls_list app\n",
			true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if v := hasPolicyEntries(s.list); v != s.expected {
				t.Fatalf("Expected %v, got %v", s.expected, v)
			}
		})
	}
}
