package model

import "testing"

// TestAvatarShard pins the integer arithmetic behind the avatar directory
// layout. The shard is id>>14, i.e. integer division by 16384, which is what
// bounds any single directory to 16384 entries instead of 10M.
func TestAvatarShard(t *testing.T) {
	const shardSize uint64 = 1 << avatarShardShift

	if shardSize != 16384 {
		t.Fatalf("shard size = %d, want 16384; avatarShardShift changed and the "+
			"directory layout of already-uploaded avatars is now wrong", shardSize)
	}

	tests := []struct {
		name string
		id   uint64
		want uint64
	}{
		{"id zero", 0, 0},
		{"first real id", 1, 0},
		{"last id in shard 0", shardSize - 1, 0},
		{"first id in shard 1", shardSize, 1},
		{"last id in shard 1", 2*shardSize - 1, 1},
		{"first id in shard 2", 2 * shardSize, 2},
		{"ten millionth user", 10_000_000, 610},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.id >> avatarShardShift; got != tt.want {
				t.Errorf("shard(%d) = %d, want %d", tt.id, got, tt.want)
			}
		})
	}
}

// TestAvatarShardIsContiguous checks the property the layout actually relies
// on: every shard holds exactly shardSize consecutive ids, with no gaps and no
// overlap at the boundaries.
func TestAvatarShardIsContiguous(t *testing.T) {
	const shardSize uint64 = 1 << avatarShardShift

	for shard := uint64(0); shard < 4; shard++ {
		first := shard * shardSize
		last := first + shardSize - 1

		if got := first >> avatarShardShift; got != shard {
			t.Errorf("first id %d of shard %d landed in shard %d", first, shard, got)
		}
		if got := last >> avatarShardShift; got != shard {
			t.Errorf("last id %d of shard %d landed in shard %d", last, shard, got)
		}
		if got := (last + 1) >> avatarShardShift; got != shard+1 {
			t.Errorf("id %d should start shard %d, landed in shard %d", last+1, shard+1, got)
		}
	}
}

func TestAvatarURL(t *testing.T) {
	tests := []struct {
		name string
		user User
		want string
	}{
		{
			name: "no avatar uploaded",
			user: User{ID: 42},
			want: DefaultAvatarURL,
		},
		{
			// An empty suffix is the single source of truth for "this user has
			// no avatar" — there is no separate version column to check.
			name: "empty suffix means no avatar",
			user: User{ID: 42, AvatarSuffix: ""},
			want: DefaultAvatarURL,
		},
		{
			name: "first upload, shard 0",
			user: User{ID: 1, AvatarSuffix: "deadbeef"},
			want: "/uploads/0/1/1_deadbeef.jpg",
		},
		{
			name: "last id in shard 0",
			user: User{ID: 16383, AvatarSuffix: "deadbeef"},
			want: "/uploads/0/16383/16383_deadbeef.jpg",
		},
		{
			name: "first id in shard 1",
			user: User{ID: 16384, AvatarSuffix: "deadbeef"},
			want: "/uploads/1/16384/16384_deadbeef.jpg",
		},
		{
			// The suffix alone is what busts caches now: a re-upload gets a
			// fresh random suffix and therefore a new URL, with no version
			// number involved anywhere.
			name: "re-upload gets a fresh suffix, not a bumped version",
			user: User{ID: 16384, AvatarSuffix: "cafef00d"},
			want: "/uploads/1/16384/16384_cafef00d.jpg",
		},
		{
			name: "ten millionth user",
			user: User{ID: 10_000_000, AvatarSuffix: "cafef00d"},
			want: "/uploads/610/10000000/10000000_cafef00d.jpg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.AvatarURL(); got != tt.want {
				t.Errorf("AvatarURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
