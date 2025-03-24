package glance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	redditWidgetHorizontalCardsTemplate = mustParseTemplate("reddit-horizontal-cards.html", "widget-base.html")
	redditWidgetVerticalCardsTemplate   = mustParseTemplate("reddit-vertical-cards.html", "widget-base.html")
)

type redditWidget struct {
	logger              *slog.Logger
	widgetBase          `yaml:",inline"`
	redditAccessToken   string
	redditAppName       string            `yaml:"reddit-app-name"`
	redditClientID      string            `yaml:"reddit-client-id"`
	redditClientSecret  string            `yaml:"reddit-client-secret"`
	Posts               forumPostList     `yaml:"-"`
	Subreddit           string            `yaml:"subreddit"`
	Proxy               proxyOptionsField `yaml:"proxy"`
	Style               string            `yaml:"style"`
	ShowThumbnails      bool              `yaml:"show-thumbnails"`
	ShowFlairs          bool              `yaml:"show-flairs"`
	SortBy              string            `yaml:"sort-by"`
	TopPeriod           string            `yaml:"top-period"`
	Search              string            `yaml:"search"`
	ExtraSortBy         string            `yaml:"extra-sort-by"`
	CommentsUrlTemplate string            `yaml:"comments-url-template"`
	Limit               int               `yaml:"limit"`
	CollapseAfter       int               `yaml:"collapse-after"`
	RequestUrlTemplate  string            `yaml:"request-url-template"`
}

type redditTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

func (widget *redditWidget) fetchRedditAccessToken() error {
	// Only execute if a matching configuration is provider
	if widget.redditAppName == "" || widget.redditClientID == "" || widget.redditClientSecret == "" {
		return nil
	}

	widget.logger.Info("Found reddit API credentials", "app-name", widget.redditAppName, "client-id", widget.redditClientID, "client-secret", widget.redditClientSecret)

	auth := base64.StdEncoding.EncodeToString([]byte(widget.redditClientID + ":" + widget.redditClientSecret))

	// Prepare form data
	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	// Create request
	req, err := http.NewRequest("POST", "https://www.reddit.com/api/v1/access_token", strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}

	// Set headers
	req.Header.Add("Authorization", "Basic "+auth)
	req.Header.Add("User-Agent", fmt.Sprintf("%s/1.0", widget.redditAppName))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("querying Reddit API: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	// Check for error status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse JSON response
	var tokenResp redditTokenResponse
	err = json.Unmarshal(body, &tokenResp)
	if err != nil {
		return fmt.Errorf("unmarshalling Reddit API response: %w", err)
	}

	widget.redditAccessToken = tokenResp.AccessToken

	widget.logger.Info("Successfully fetched Reddit access token", "access-token", tokenResp.AccessToken)

	return nil
}

func (widget *redditWidget) initialize() error {
	if widget.Subreddit == "" {
		return errors.New("subreddit is required")
	}

	if widget.Limit <= 0 {
		widget.Limit = 15
	}

	if widget.CollapseAfter == 0 || widget.CollapseAfter < -1 {
		widget.CollapseAfter = 5
	}

	if !isValidRedditSortType(widget.SortBy) {
		widget.SortBy = "hot"
	}

	if !isValidRedditTopPeriod(widget.TopPeriod) {
		widget.TopPeriod = "day"
	}

	if widget.RequestUrlTemplate != "" {
		if !strings.Contains(widget.RequestUrlTemplate, "{REQUEST-URL}") {
			return errors.New("no `{REQUEST-URL}` placeholder specified")
		}
	}

	widget.logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := widget.fetchRedditAccessToken(); err != nil {
		return fmt.Errorf("fetching Reddit API access token: %w", err)
	}

	widget.
		withTitle("r/" + widget.Subreddit).
		withTitleURL("https://www.reddit.com/r/" + widget.Subreddit + "/").
		withCacheDuration(30 * time.Minute)

	return nil
}

func isValidRedditSortType(sortBy string) bool {
	return sortBy == "hot" ||
		sortBy == "new" ||
		sortBy == "top" ||
		sortBy == "rising"
}

func isValidRedditTopPeriod(period string) bool {
	return period == "hour" ||
		period == "day" ||
		period == "week" ||
		period == "month" ||
		period == "year" ||
		period == "all"
}

func (widget *redditWidget) update(ctx context.Context) {
	// TODO: refactor, use a struct to pass all of these
	posts, err := fetchSubredditPosts(
		widget.Subreddit,
		widget.SortBy,
		widget.TopPeriod,
		widget.Search,
		widget.CommentsUrlTemplate,
		widget.RequestUrlTemplate,
		widget.Proxy.client,
		widget.ShowFlairs,
		widget.redditAppName,
		widget.redditAccessToken,
	)

	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if len(posts) > widget.Limit {
		posts = posts[:widget.Limit]
	}

	if widget.ExtraSortBy == "engagement" {
		posts.calculateEngagement()
		posts.sortByEngagement()
	}

	widget.Posts = posts
}

func (widget *redditWidget) Render() template.HTML {
	if widget.Style == "horizontal-cards" {
		return widget.renderTemplate(widget, redditWidgetHorizontalCardsTemplate)
	}

	if widget.Style == "vertical-cards" {
		return widget.renderTemplate(widget, redditWidgetVerticalCardsTemplate)
	}

	return widget.renderTemplate(widget, forumPostsTemplate)

}

type subredditResponseJson struct {
	Data struct {
		Children []struct {
			Data struct {
				Id            string  `json:"id"`
				Title         string  `json:"title"`
				Upvotes       int     `json:"ups"`
				Url           string  `json:"url"`
				Time          float64 `json:"created"`
				CommentsCount int     `json:"num_comments"`
				Domain        string  `json:"domain"`
				Permalink     string  `json:"permalink"`
				Stickied      bool    `json:"stickied"`
				Pinned        bool    `json:"pinned"`
				IsSelf        bool    `json:"is_self"`
				Thumbnail     string  `json:"thumbnail"`
				Flair         string  `json:"link_flair_text"`
				ParentList    []struct {
					Id        string `json:"id"`
					Subreddit string `json:"subreddit"`
					Permalink string `json:"permalink"`
				} `json:"crosspost_parent_list"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

func templateRedditCommentsURL(template, subreddit, postId, postPath string) string {
	template = strings.ReplaceAll(template, "{SUBREDDIT}", subreddit)
	template = strings.ReplaceAll(template, "{POST-ID}", postId)
	template = strings.ReplaceAll(template, "{POST-PATH}", strings.TrimLeft(postPath, "/"))

	return template
}

func fetchSubredditPosts(
	subreddit,
	sort,
	topPeriod,
	search,
	commentsUrlTemplate,
	requestUrlTemplate string,
	proxyClient *http.Client,
	showFlairs bool,
	redditAppName string,
	redditAccessToken string,
) (forumPostList, error) {
	query := url.Values{}
	var requestUrl string

	if search != "" {
		query.Set("q", search+" subreddit:"+subreddit)
		query.Set("sort", sort)
	}

	if sort == "top" {
		query.Set("t", topPeriod)
	}

	var baseURL string

	if redditAccessToken != "" {
		baseURL = "https://oauth.reddit.com"
	} else {
		baseURL = "https://www.reddit.com"
	}

	if search != "" {
		requestUrl = fmt.Sprintf("%s/search.json?%s", baseURL, query.Encode())
	} else {
		requestUrl = fmt.Sprintf("%s/r/%s/%s.json?%s", baseURL, subreddit, sort, query.Encode())
	}

	var client requestDoer = defaultHTTPClient

	if requestUrlTemplate != "" {
		requestUrl = strings.ReplaceAll(requestUrlTemplate, "{REQUEST-URL}", requestUrl)
	} else if proxyClient != nil {
		client = proxyClient
	}

	request, err := http.NewRequest("GET", requestUrl, nil)
	if err != nil {
		return nil, err
	}

	// Required to increase rate limit, otherwise Reddit randomly returns 429 even after just 2 requests
	if redditAppName == "" {
		setBrowserUserAgentHeader(request)
	} else {
		request.Header.Set("User-Agent", fmt.Sprintf("%s/1.0", redditAppName))
	}

	if redditAccessToken != "" {
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", redditAccessToken))
	}

	responseJson, err := decodeJsonFromRequest[subredditResponseJson](client, request)
	if err != nil {
		return nil, err
	}

	if len(responseJson.Data.Children) == 0 {
		return nil, fmt.Errorf("no posts found")
	}

	posts := make(forumPostList, 0, len(responseJson.Data.Children))

	for i := range responseJson.Data.Children {
		post := &responseJson.Data.Children[i].Data

		if post.Stickied || post.Pinned {
			continue
		}

		var commentsUrl string

		if commentsUrlTemplate == "" {
			commentsUrl = "https://www.reddit.com" + post.Permalink
		} else {
			commentsUrl = templateRedditCommentsURL(commentsUrlTemplate, subreddit, post.Id, post.Permalink)
		}

		forumPost := forumPost{
			Title:           html.UnescapeString(post.Title),
			DiscussionUrl:   commentsUrl,
			TargetUrlDomain: post.Domain,
			CommentCount:    post.CommentsCount,
			Score:           post.Upvotes,
			TimePosted:      time.Unix(int64(post.Time), 0),
		}

		if post.Thumbnail != "" && post.Thumbnail != "self" && post.Thumbnail != "default" && post.Thumbnail != "nsfw" {
			forumPost.ThumbnailUrl = html.UnescapeString(post.Thumbnail)
		}

		if !post.IsSelf {
			forumPost.TargetUrl = post.Url
		}

		if showFlairs && post.Flair != "" {
			forumPost.Tags = append(forumPost.Tags, post.Flair)
		}

		if len(post.ParentList) > 0 {
			forumPost.IsCrosspost = true
			forumPost.TargetUrlDomain = "r/" + post.ParentList[0].Subreddit

			if commentsUrlTemplate == "" {
				forumPost.TargetUrl = "https://www.reddit.com" + post.ParentList[0].Permalink
			} else {
				forumPost.TargetUrl = templateRedditCommentsURL(
					commentsUrlTemplate,
					post.ParentList[0].Subreddit,
					post.ParentList[0].Id,
					post.ParentList[0].Permalink,
				)
			}
		}

		posts = append(posts, forumPost)
	}

	return posts, nil
}
