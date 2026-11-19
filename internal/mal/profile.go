package mal

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"

	"kotantei/internal/notification"
	"kotantei/internal/store"
	"kotantei/internal/update"
)

var (
	ErrMalformed           = errors.New("MAL returned malformed or incomplete public data")
	ErrResponseBudget      = errors.New("MAL response exceeded its size budget")
	ErrIncompleteFavorites = errors.New("MAL favorites are incomplete; the saved baseline was preserved")
)

var favoriteCount = regexp.MustCompile(`^([^()]+)\s*\(([0-9]+)\)$`)
var imageResize = regexp.MustCompile(`/r/[0-9]+x[0-9]+`)

func parseProfile(body []byte, favorites bool) (Profile, error) {
	document, err := parseHTML(body)
	if err != nil {
		return Profile{}, err
	}
	meta := findNode(document, func(n *html.Node) bool { return element(n, "meta") && attr(n, "property") == "og:url" })
	if meta == nil {
		return Profile{}, ErrMalformed
	}
	name, ok := profileName(attr(meta, "content"))
	if !ok {
		return Profile{}, ErrMalformed
	}
	wrapper := findNode(document, func(n *html.Node) bool { return attr(n, "id") == "contentWrapper" })
	if wrapper == nil {
		return Profile{}, ErrMalformed
	}
	report := findNode(wrapper, func(n *html.Node) bool { return element(n, "a") && hasClass(n, "header-right") && ancestor(n, "h1") })
	if report == nil {
		return Profile{}, ErrMalformed
	}
	reportURL, err := siteURL(attr(report, "href"))
	if err != nil || reportURL.Path != "/modules.php" || reportURL.Query().Get("go") != "report" || reportURL.Query().Get("type") != "profile" {
		return Profile{}, ErrMalformed
	}
	id, err := strconv.ParseInt(reportURL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != reportURL.Query().Get("id") {
		return Profile{}, ErrMalformed
	}
	profile := Profile{ID: id, Username: name, AvatarURL: avatarURL(id)}
	avatar := findNode(wrapper, func(n *html.Node) bool { return hasClass(n, "user-image") })
	if avatar != nil {
		image := findNode(avatar, func(n *html.Node) bool { return element(n, "img") })
		if image != nil {
			for _, key := range []string{"data-src", "src"} {
				if image := cdnImage(attr(image, key)); image != "" {
					profile.AvatarURL = image
					break
				}
			}
		}
	}
	if favorites {
		profile.Favorites, profile.FavoritesComplete = parseFavorites(wrapper, name, body)
	}
	return profile, nil
}

func parseComments(body []byte) (string, error) {
	document, err := parseHTML(body)
	if err != nil {
		return "", err
	}
	content := findNode(document, func(n *html.Node) bool { return attr(n, "id") == "content" })
	if content == nil {
		return "", ErrMalformed
	}
	for header := content.FirstChild; header != nil; header = header.NextSibling {
		if !element(header, "div") || !hasClass(header, "borderClass") {
			continue
		}
		for group := header.FirstChild; group != nil; group = group.NextSibling {
			if !element(group, "div") {
				continue
			}
			for link := group.FirstChild; link != nil; link = link.NextSibling {
				if element(link, "a") {
					if name, ok := profileName(attr(link, "href")); ok {
						return name, nil
					}
					return "", ErrMalformed
				}
			}
			return "", ErrMalformed
		}
		return "", ErrMalformed
	}
	return "", ErrMalformed
}

func parseFavorites(wrapper *html.Node, name string, body []byte) ([]store.Favorite, bool) {
	// A preview/count mismatch or a truncated document is not an empty set.
	lower := bytes.ToLower(body)
	if !bytes.Contains(lower, []byte("</body>")) || !bytes.Contains(lower, []byte("</html>")) {
		return nil, false
	}
	header := findNode(wrapper, func(n *html.Node) bool {
		if !element(n, "h2") {
			return false
		}
		return findNode(n, func(link *html.Node) bool {
			if !element(link, "a") {
				return false
			}
			u, err := siteURL(attr(link, "href"))
			return err == nil && u.Path == "/profile/"+name+"/favorites"
		}) != nil
	})
	if header == nil {
		return nil, false
	}
	expected := map[update.SubjectKind]int{}
	sections := map[update.SubjectKind]*html.Node{}
	for node := header.NextSibling; node != nil; node = node.NextSibling {
		if element(node, "h2") {
			break
		}
		if element(node, "h5") {
			match := favoriteCount.FindStringSubmatch(strings.TrimSpace(nodeText(node)))
			if len(match) != 3 {
				return nil, false
			}
			kind, ok := favoriteLabel(strings.TrimSpace(match[1]))
			if !ok {
				return nil, false
			}
			count, err := strconv.Atoi(match[2])
			if err != nil || count > 5000 {
				return nil, false
			}
			if _, duplicate := expected[kind]; duplicate {
				return nil, false
			}
			expected[kind] = count
		}
		for _, pair := range []struct {
			id   string
			kind update.SubjectKind
		}{{"anime_favorites", update.AnimeSubject}, {"manga_favorites", update.MangaSubject}, {"character_favorites", update.CharacterSubject}, {"person_favorites", update.PersonSubject}, {"company_favorites", update.CompanySubject}} {
			section := findNode(node, func(n *html.Node) bool { return attr(n, "id") == pair.id })
			if section != nil {
				if sections[pair.kind] != nil {
					return nil, false
				}
				sections[pair.kind] = section
			}
		}
	}
	result := []store.Favorite{}
	seen := map[update.Subject]bool{}
	for _, kind := range []update.SubjectKind{update.AnimeSubject, update.MangaSubject, update.CharacterSubject, update.PersonSubject, update.CompanySubject} {
		section := sections[kind]
		count := 0
		if section != nil {
			if _, ok := expected[kind]; !ok {
				return nil, false
			}
			valid := true
			walkNodes(section, func(n *html.Node) {
				if !valid || !element(n, "li") || !hasClass(n, "btn-fav") {
					return
				}
				link := findNode(n, func(n *html.Node) bool { return element(n, "a") })
				if link == nil {
					valid = false
					return
				}
				id, ok := favoriteID(attr(link, "href"), kind)
				if !ok {
					valid = false
					return
				}
				subject := update.Subject{Kind: kind, ID: id}
				if seen[subject] || len(result) >= 5000 {
					valid = false
					return
				}
				seen[subject] = true
				favorite := store.Favorite{Subject: subject, NameURL: favoriteURL(subject)}
				title := findNode(link, func(n *html.Node) bool { return hasClass(n, "title") })
				if title != nil {
					favorite.Name = strings.TrimSpace(nodeText(title))
				}
				if favorite.Name == "" {
					favorite.Name = string(kind) + " #" + id
				}
				image := findNode(link, func(n *html.Node) bool { return element(n, "img") })
				if image != nil {
					favorite.ImageURL = cdnImage(attr(image, "data-src"))
					if favorite.ImageURL == "" {
						favorite.ImageURL = cdnImage(attr(image, "src"))
					}
				}
				details := findNode(link, func(n *html.Node) bool { return hasClass(n, "users") })
				extra := FavoriteMetadata{}
				if details != nil {
					text := strings.TrimSpace(nodeText(details))
					if kind == update.CharacterSubject {
						extra.From = text
					} else if kind == update.AnimeSubject || kind == update.MangaSubject {
						parts := strings.SplitN(text, "·", 2)
						extra.Format = strings.TrimSpace(parts[0])
						if len(parts) == 2 {
							year, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
							if year > 0 && year <= 9999 {
								extra.Year = year
							}
						}
					}
				}
				favorite.Extra, _ = json.Marshal(extra)
				result = append(result, favorite)
				count++
			})
			if !valid {
				return nil, false
			}
		}
		if count != expected[kind] {
			return nil, false
		}
	}
	return result, true
}

func parseHTML(body []byte) (*html.Node, error) {
	if len(body) > 1<<20 {
		return nil, ErrResponseBudget
	}
	if !utf8.Valid(body) {
		return nil, ErrMalformed
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, ErrMalformed
	}
	return document, nil
}
func attr(n *html.Node, key string) string {
	if n != nil {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
	}
	return ""
}
func element(n *html.Node, name string) bool {
	return n != nil && n.Type == html.ElementNode && n.Data == name
}
func hasClass(n *html.Node, name string) bool {
	for _, class := range strings.Fields(attr(n, "class")) {
		if class == name {
			return true
		}
	}
	return false
}
func ancestor(n *html.Node, name string) bool {
	for n = n.Parent; n != nil; n = n.Parent {
		if element(n, name) {
			return true
		}
	}
	return false
}
func walkNodes(n *html.Node, visit func(*html.Node)) {
	if n == nil {
		return
	}
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walkNodes(child, visit)
	}
}
func findNode(n *html.Node, test func(*html.Node) bool) *html.Node {
	if n == nil {
		return nil
	}
	if test(n) {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findNode(child, test); found != nil {
			return found
		}
	}
	return nil
}
func nodeText(n *html.Node) string {
	var text strings.Builder
	walkNodes(n, func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
	})
	return text.String()
}
func validUsername(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
func siteURL(raw string) (*url.URL, error) {
	base, _ := url.Parse("https://myanimelist.net")
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrMalformed
	}
	u = base.ResolveReference(u)
	if u.Scheme != "https" || u.User != nil || (u.Host != "myanimelist.net" && u.Host != "www.myanimelist.net") {
		return nil, ErrMalformed
	}
	return u, nil
}
func profileName(raw string) (string, bool) {
	u, err := siteURL(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "profile" || !validUsername(parts[1]) {
		return "", false
	}
	return parts[1], true
}
func avatarURL(id int64) string {
	return "https://cdn.myanimelist.net/images/userimages/" + strconv.FormatInt(id, 10) + ".jpg"
}
func cdnImage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base, _ := url.Parse("https://myanimelist.net")
	u = base.ResolveReference(u)
	if u.Hostname() != "cdn.myanimelist.net" || strings.Contains(u.Path, "spacer") {
		return ""
	}
	u.Path = imageResize.ReplaceAllString(u.Path, "")
	return notification.SafeURL(u.String())
}
func favoriteLabel(label string) (update.SubjectKind, bool) {
	switch strings.ToLower(label) {
	case "anime":
		return update.AnimeSubject, true
	case "manga":
		return update.MangaSubject, true
	case "characters":
		return update.CharacterSubject, true
	case "people":
		return update.PersonSubject, true
	case "companies", "studios":
		return update.CompanySubject, true
	}
	return "", false
}
func favoriteID(raw string, kind update.SubjectKind) (string, bool) {
	u, err := siteURL(raw)
	if err != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	index := 1
	path := string(kind)
	if kind == update.PersonSubject {
		path = "people"
	}
	if kind == update.CompanySubject {
		if len(parts) < 3 || parts[0] != "anime" || parts[1] != "producer" {
			return "", false
		}
		index = 2
	} else if len(parts) < 2 || parts[0] != path {
		return "", false
	}
	id := parts[index]
	number, err := strconv.ParseUint(id, 10, 64)
	return id, err == nil && number > 0 && strconv.FormatUint(number, 10) == id
}
func favoriteURL(subject update.Subject) string {
	kind := string(subject.Kind)
	if subject.Kind == update.PersonSubject {
		kind = "people"
	}
	if subject.Kind == update.CompanySubject {
		kind = "anime/producer"
	}
	return "https://myanimelist.net/" + kind + "/" + subject.ID
}
