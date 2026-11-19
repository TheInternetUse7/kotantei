package mal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kotantei/internal/update"
)

func profileHTML(id int64, name, favorites string) string {
	return fmt.Sprintf(`<html><head><meta property="og:url" content="https://myanimelist.net/profile/%s"></head><body>
<div id="contentWrapper"><h1><a class="header-right mt4" href="/modules.php?go=report&amp;type=profile&amp;id=%d">Report</a></h1>
<div class="user-image mb8"><img src="https://cdn.myanimelist.net/images/spacer.gif" data-src="https://cdn.myanimelist.net/s/common/userimages/avatar_225w"></div>
<h2><a href="/profile/%s/favorites">All Favorites</a>Favorites</h2>%s
</div></body></html>`, name, id, name, favorites)
}

func favoriteSection(category, label, path, name, details string) string {
	return fmt.Sprintf(`<h5>%s (1)</h5><div id="%s_favorites"><div class="fav-slide-outer"><ul><li class="btn-fav"><a href="%s"><span class="title">%s</span><span class="users">%s</span><img class="image lazyload" src="https://cdn.myanimelist.net/images/spacer.gif" data-src="https://cdn.myanimelist.net/r/140x220/images/sample.jpg"></a></li></ul></div></div>`, label, category, path, name, details)
}

func TestProfileParserResolvesStableIdentityAndCurrentLazyAvatar(t *testing.T) {
	profile, err := parseProfile([]byte(profileHTML(101, "Reader", "")), true)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != 101 || profile.Username != "Reader" || profile.AvatarURL != "https://cdn.myanimelist.net/s/common/userimages/avatar_225w" || !profile.FavoritesComplete || len(profile.Favorites) != 0 {
		t.Fatalf("wrong public identity: %+v", profile)
	}
}

func TestProfileParserCollectsAllFiveFavoriteCategories(t *testing.T) {
	sections := []string{
		favoriteSection("anime", "Anime", "/anime/42/Title", "Anime &amp; title", "TV&middot;2020"),
		favoriteSection("manga", "Manga", "https://myanimelist.net/manga/43/Title", "Manga", "Manga&middot;2010"),
		favoriteSection("character", "Characters", "/character/44/Name", "Character", "From title"),
		favoriteSection("person", "People", "/people/45/Name", "Person", ""),
		favoriteSection("company", "Companies", "/anime/producer/46/Name", "Company", ""),
	}
	profile, err := parseProfile([]byte(profileHTML(101, "Reader", strings.Join(sections, ""))), true)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.FavoritesComplete || len(profile.Favorites) != 5 {
		t.Fatalf("incomplete favorites: %+v", profile)
	}
	want := map[update.SubjectKind]string{update.AnimeSubject: "42", update.MangaSubject: "43", update.CharacterSubject: "44", update.PersonSubject: "45", update.CompanySubject: "46"}
	for _, favorite := range profile.Favorites {
		if favorite.Subject.ID != want[favorite.Subject.Kind] || favorite.Name == "" || favorite.ImageURL != "https://cdn.myanimelist.net/images/sample.jpg" {
			t.Fatalf("invalid favorite: %+v", favorite)
		}
		var extra FavoriteMetadata
		if err := json.Unmarshal(favorite.Extra, &extra); err != nil {
			t.Fatal(err)
		}
		if favorite.Subject.Kind == update.AnimeSubject && (favorite.Name != "Anime & title" || extra.Format != "TV" || extra.Year != 2020) {
			t.Fatal("media favorite details lost")
		}
		if favorite.Subject.Kind == update.CharacterSubject && extra.From != "From title" {
			t.Fatal("character source lost")
		}
	}
}

func TestPartialFavoriteHTMLNeverBecomesAnEmptySnapshot(t *testing.T) {
	full := profileHTML(101, "Reader", favoriteSection("anime", "Anime", "/anime/42", "Title", "TV&middot;2020"))
	for _, body := range []string{
		strings.Replace(full, "Anime (1)", "Anime (10)", 1),
		strings.Replace(full, "</body></html>", "", 1),
		strings.Replace(full, "<h2>", "<h3>", 1),
		strings.Replace(full, "/anime/42", "/manga/42", 1),
		strings.Replace(full, "/anime/42", "https://untrusted.example/anime/42", 1),
	} {
		profile, err := parseProfile([]byte(body), true)
		if err != nil {
			t.Fatal("favorite damage prevented parsing the independent identity", err)
		}
		if profile.FavoritesComplete {
			t.Fatal("incomplete favorites accepted as a replacement baseline")
		}
	}
}

func TestProfileParserRejectsChallengesAndUntrustedIdentities(t *testing.T) {
	good := profileHTML(101, "Reader", "")
	for _, body := range []string{
		`<html><body>Verify you are human</body></html>`,
		strings.Replace(good, "type=profile", "type=other", 1),
		strings.Replace(good, "id=101", "id=0", 1),
		strings.Replace(good, "https://myanimelist.net/profile/Reader", "https://untrusted.example/profile/Reader", 1),
		strings.Replace(good, "https://myanimelist.net/profile/Reader", "https://myanimelist.net/profile/@me", 1),
	} {
		if _, err := parseProfile([]byte(body), false); !errors.Is(err, ErrMalformed) {
			t.Fatalf("untrusted profile accepted: %v", err)
		}
	}
	if _, err := parseProfile([]byte(strings.Repeat("x", (1<<20)+1)), true); !errors.Is(err, ErrResponseBudget) {
		t.Fatal("unbounded HTML accepted", err)
	}
}

func TestMissingOrUnsafeAvatarsUseAnIDBasedCDNFallback(t *testing.T) {
	body := strings.Replace(profileHTML(101, "Reader", ""), "https://cdn.myanimelist.net/s/common/userimages/avatar_225w", "file:///not-public", 1)
	profile, err := parseProfile([]byte(body), false)
	if err != nil {
		t.Fatal(err)
	}
	if profile.AvatarURL != "https://cdn.myanimelist.net/images/userimages/101.jpg" {
		t.Fatal("avatar fallback was not isolated from profile identity")
	}
}

func TestCommentsParserUsesTheOwnerHeaderNotCommenterProfiles(t *testing.T) {
	body := `<html><body><div id="content"><div class="borderClass"><div><a href="/profile/Renamed">Renamed's Profile</a></div><div>Pages</div></div><div class="comment"><a href="/profile/Other">Other</a></div></div></body></html>`
	name, err := parseComments([]byte(body))
	if err != nil || name != "Renamed" {
		t.Fatalf("wrong recovered owner: %q %v", name, err)
	}
	for _, body := range []string{`<div class="comment"><a href="/profile/Other">Other</a></div>`, strings.Replace(body, "/profile/Renamed", "https://untrusted.example/profile/Renamed", 1)} {
		if _, err := parseComments([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Fatal("commenter or cross-origin owner was trusted", err)
		}
	}
}
