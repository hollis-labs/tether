package provider

import (
	"reflect"
	"testing"
)

func TestBuildEnv_MergeDefault_InheritsParent(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/user", "SHELL=/bin/zsh"}
	got := BuildEnv(EnvModeMerge, nil, nil, nil, parent)
	want := []string{"HOME=/home/user", "PATH=/usr/bin", "SHELL=/bin/zsh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge with no overrides/redact should pass parent through sorted\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_EmptyMode_DefaultsToMerge(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/user"}
	got := BuildEnv("", nil, nil, nil, parent)
	want := []string{"HOME=/home/user", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty mode should behave as merge\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_UnknownMode_DefaultsToMerge(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/user"}
	got := BuildEnv("strict", nil, nil, nil, parent)
	want := []string{"HOME=/home/user", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown mode should behave as merge\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_MergeOverrides(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "FOO=old"}
	overrides := map[string]string{"FOO": "new", "BAR": "bar-val"}
	got := BuildEnv(EnvModeMerge, nil, nil, overrides, parent)
	want := []string{"BAR=bar-val", "FOO=new", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overrides should win over parent and add new keys\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_MergeRedactDropsKeys(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "SECRET=shh", "OTHER=ok"}
	got := BuildEnv(EnvModeMerge, nil, []string{"SECRET"}, nil, parent)
	want := []string{"OTHER=ok", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redact should drop listed keys from merged result\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_MergeRedactThenOverrideReintroducesKey(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "SECRET=shh"}
	overrides := map[string]string{"SECRET": "override"}
	got := BuildEnv(EnvModeMerge, nil, []string{"SECRET"}, overrides, parent)
	want := []string{"PATH=/usr/bin", "SECRET=override"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit override after redact should reintroduce the key\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_Whitelist_OnlyListedKeys(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/user", "SECRET=shh", "EXTRA=x"}
	got := BuildEnv(EnvModeWhitelist, []string{"PATH", "HOME"}, nil, nil, parent)
	want := []string{"HOME=/home/user", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("whitelist should include only listed keys\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_Whitelist_OverridesWin(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/user"}
	overrides := map[string]string{"PATH": "/override/bin", "NEW": "added"}
	got := BuildEnv(EnvModeWhitelist, []string{"PATH", "HOME"}, nil, overrides, parent)
	want := []string{"HOME=/home/user", "NEW=added", "PATH=/override/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("whitelist overrides should win and add new keys\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_Whitelist_MissingPassthroughKeyOmitted(t *testing.T) {
	parent := []string{"PATH=/usr/bin"}
	got := BuildEnv(EnvModeWhitelist, []string{"PATH", "NOT_IN_PARENT"}, nil, nil, parent)
	want := []string{"PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing passthrough keys should be silently omitted (not empty-valued)\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_ParentEntryWithoutEquals_Ignored(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "malformed-no-equals", "=no-key"}
	got := BuildEnv(EnvModeMerge, nil, nil, nil, parent)
	want := []string{"PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("malformed parent entries should be ignored\n got=%v\nwant=%v", got, want)
	}
}

func TestBuildEnv_ValueMayContainEquals(t *testing.T) {
	parent := []string{"URL=https://example.com/?q=1&r=2"}
	got := BuildEnv(EnvModeMerge, nil, nil, nil, parent)
	want := []string{"URL=https://example.com/?q=1&r=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values containing = should survive intact\n got=%v\nwant=%v", got, want)
	}
}
