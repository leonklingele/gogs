// Copyright 2016 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"fmt"
	"strings"
	"time"

	"github.com/Unknwon/com"
	"github.com/go-xorm/xorm"

	api "github.com/gogits/go-gogs-client"

	"github.com/gogits/gogs/modules/log"
	"github.com/gogits/gogs/modules/markdown"
)

// CommentType defines whether a comment is just a simple comment, an action (like close) or a reference.
type CommentType int

const (
	// Plain comment, can be associated with a commit (CommitID > 0) and a line (LineNum > 0)
	COMMENT_TYPE_COMMENT CommentType = iota
	COMMENT_TYPE_REOPEN
	COMMENT_TYPE_CLOSE

	// References.
	COMMENT_TYPE_ISSUE_REF
	// Reference from a commit (not part of a pull request)
	COMMENT_TYPE_COMMIT_REF
	// Reference from a comment
	COMMENT_TYPE_COMMENT_REF
	// Reference from a pull request
	COMMENT_TYPE_PULL_REF
)

type CommentTag int

const (
	COMMENT_TAG_NONE CommentTag = iota
	COMMENT_TAG_POSTER
	COMMENT_TAG_WRITER
	COMMENT_TAG_OWNER
)

// Comment represents a comment in commit and issue page.
type Comment struct {
	ID              int64 `xorm:"pk autoincr"`
	Type            CommentType
	PosterID        int64
	Poster          *User `xorm:"-"`
	IssueID         int64 `xorm:"INDEX"`
	CommitID        int64
	Line            int64
	Content         string `xorm:"TEXT"`
	RenderedContent string `xorm:"-"`

	Created     time.Time `xorm:"-"`
	CreatedUnix int64
	Updated     time.Time `xorm:"-"`
	UpdatedUnix int64

	// Reference issue in commit message
	CommitSHA string `xorm:"VARCHAR(40)"`

	Attachments []*Attachment `xorm:"-"`

	// For view issue page.
	ShowTag CommentTag `xorm:"-"`
}

func (c *Comment) BeforeInsert() {
	c.CreatedUnix = time.Now().Unix()
	c.UpdatedUnix = c.CreatedUnix
}

func (c *Comment) BeforeUpdate() {
	c.UpdatedUnix = time.Now().Unix()
}

func (c *Comment) AfterSet(colName string, _ xorm.Cell) {
	var err error
	switch colName {
	case "id":
		c.Attachments, err = GetAttachmentsByCommentID(c.ID)
		if err != nil {
			log.Error(3, "GetAttachmentsByCommentID[%d]: %v", c.ID, err)
		}

	case "poster_id":
		c.Poster, err = GetUserByID(c.PosterID)
		if err != nil {
			if IsErrUserNotExist(err) {
				c.PosterID = -1
				c.Poster = NewGhostUser()
			} else {
				log.Error(3, "GetUserByID[%d]: %v", c.ID, err)
			}
		}
	case "created_unix":
		c.Created = time.Unix(c.CreatedUnix, 0).Local()
	case "updated_unix":
		c.Updated = time.Unix(c.UpdatedUnix, 0).Local()
	}
}

func (c *Comment) AfterDelete() {
	_, err := DeleteAttachmentsByComment(c.ID, true)

	if err != nil {
		log.Info("Could not delete files for comment %d on issue #%d: %s", c.ID, c.IssueID, err)
	}
}

func (c *Comment) APIFormat() *api.Comment {
	return &api.Comment{
		ID:      c.ID,
		Poster:  c.Poster.APIFormat(),
		Body:    c.Content,
		Created: c.Created,
		Updated: c.Updated,
	}
}

// HashTag returns unique hash tag for comment.
func (c *Comment) HashTag() string {
	return "issuecomment-" + com.ToStr(c.ID)
}

// EventTag returns unique event hash tag for comment.
func (c *Comment) EventTag() string {
	return "event-" + com.ToStr(c.ID)
}

// MailParticipants sends new comment emails to repository watchers
// and mentioned people.
func (cmt *Comment) MailParticipants(opType ActionType, issue *Issue) (err error) {
	mentions := markdown.FindAllMentions(cmt.Content)
	if err = UpdateIssueMentions(cmt.IssueID, mentions); err != nil {
		return fmt.Errorf("UpdateIssueMentions [%d]: %v", cmt.IssueID, err)
	}

	switch opType {
	case ACTION_COMMENT_ISSUE:
		issue.Content = cmt.Content
	case ACTION_CLOSE_ISSUE:
		issue.Content = fmt.Sprintf("Closed #%d", issue.Index)
	case ACTION_REOPEN_ISSUE:
		issue.Content = fmt.Sprintf("Reopened #%d", issue.Index)
	}
	if err = mailIssueCommentToParticipants(issue, cmt.Poster, mentions); err != nil {
		log.Error(4, "mailIssueCommentToParticipants: %v", err)
	}

	return nil
}

func createComment(e *xorm.Session, opts *CreateCommentOptions) (_ *Comment, err error) {
	comment := &Comment{
		Type:      opts.Type,
		PosterID:  opts.Doer.ID,
		Poster:    opts.Doer,
		IssueID:   opts.Issue.ID,
		CommitID:  opts.CommitID,
		CommitSHA: opts.CommitSHA,
		Line:      opts.LineNum,
		Content:   opts.Content,
	}
	if _, err = e.Insert(comment); err != nil {
		return nil, err
	}

	// Compose comment action, could be plain comment, close or reopen issue/pull request.
	// This object will be used to notify watchers in the end of function.
	act := &Action{
		ActUserID:    opts.Doer.ID,
		ActUserName:  opts.Doer.Name,
		Content:      fmt.Sprintf("%d|%s", opts.Issue.Index, strings.Split(opts.Content, "\n")[0]),
		RepoID:       opts.Repo.ID,
		RepoUserName: opts.Repo.Owner.Name,
		RepoName:     opts.Repo.Name,
		IsPrivate:    opts.Repo.IsPrivate,
	}

	// Check comment type.
	switch opts.Type {
	case COMMENT_TYPE_COMMENT:
		act.OpType = ACTION_COMMENT_ISSUE

		if _, err = e.Exec("UPDATE `issue` SET num_comments=num_comments+1 WHERE id=?", opts.Issue.ID); err != nil {
			return nil, err
		}

		// Check attachments
		attachments := make([]*Attachment, 0, len(opts.Attachments))
		for _, uuid := range opts.Attachments {
			attach, err := getAttachmentByUUID(e, uuid)
			if err != nil {
				if IsErrAttachmentNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("getAttachmentByUUID [%s]: %v", uuid, err)
			}
			attachments = append(attachments, attach)
		}

		for i := range attachments {
			attachments[i].IssueID = opts.Issue.ID
			attachments[i].CommentID = comment.ID
			// No assign value could be 0, so ignore AllCols().
			if _, err = e.Id(attachments[i].ID).Update(attachments[i]); err != nil {
				return nil, fmt.Errorf("update attachment [%d]: %v", attachments[i].ID, err)
			}
		}

	case COMMENT_TYPE_REOPEN:
		act.OpType = ACTION_REOPEN_ISSUE
		if opts.Issue.IsPull {
			act.OpType = ACTION_REOPEN_PULL_REQUEST
		}

		if opts.Issue.IsPull {
			_, err = e.Exec("UPDATE `repository` SET num_closed_pulls=num_closed_pulls-1 WHERE id=?", opts.Repo.ID)
		} else {
			_, err = e.Exec("UPDATE `repository` SET num_closed_issues=num_closed_issues-1 WHERE id=?", opts.Repo.ID)
		}
		if err != nil {
			return nil, err
		}

	case COMMENT_TYPE_CLOSE:
		act.OpType = ACTION_CLOSE_ISSUE
		if opts.Issue.IsPull {
			act.OpType = ACTION_CLOSE_PULL_REQUEST
		}

		if opts.Issue.IsPull {
			_, err = e.Exec("UPDATE `repository` SET num_closed_pulls=num_closed_pulls+1 WHERE id=?", opts.Repo.ID)
		} else {
			_, err = e.Exec("UPDATE `repository` SET num_closed_issues=num_closed_issues+1 WHERE id=?", opts.Repo.ID)
		}
		if err != nil {
			return nil, err
		}

	}

	// Notify watchers for whatever action comes in, ignore if no action type.
	if act.OpType > 0 {
		if err = notifyWatchers(e, act); err != nil {
			log.Error(4, "notifyWatchers: %v", err)
		}
		comment.MailParticipants(act.OpType, opts.Issue)
	}

	return comment, nil
}

func createStatusComment(e *xorm.Session, doer *User, repo *Repository, issue *Issue) (*Comment, error) {
	cmtType := COMMENT_TYPE_CLOSE
	if !issue.IsClosed {
		cmtType = COMMENT_TYPE_REOPEN
	}
	return createComment(e, &CreateCommentOptions{
		Type:  cmtType,
		Doer:  doer,
		Repo:  repo,
		Issue: issue,
	})
}

type CreateCommentOptions struct {
	Type  CommentType
	Doer  *User
	Repo  *Repository
	Issue *Issue

	CommitID    int64
	CommitSHA   string
	LineNum     int64
	Content     string
	Attachments []string // UUIDs of attachments
}

// CreateComment creates comment of issue or commit.
func CreateComment(opts *CreateCommentOptions) (comment *Comment, err error) {
	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return nil, err
	}

	comment, err = createComment(sess, opts)
	if err != nil {
		return nil, err
	}

	return comment, sess.Commit()
}

// CreateIssueComment creates a plain issue comment.
func CreateIssueComment(doer *User, repo *Repository, issue *Issue, content string, attachments []string) (*Comment, error) {
	return CreateComment(&CreateCommentOptions{
		Type:        COMMENT_TYPE_COMMENT,
		Doer:        doer,
		Repo:        repo,
		Issue:       issue,
		Content:     content,
		Attachments: attachments,
	})
}

// CreateRefComment creates a commit reference comment to issue.
func CreateRefComment(doer *User, repo *Repository, issue *Issue, content, commitSHA string) error {
	if len(commitSHA) == 0 {
		return fmt.Errorf("cannot create reference with empty commit SHA")
	}

	// Check if same reference from same commit has already existed.
	has, err := x.Get(&Comment{
		Type:      COMMENT_TYPE_COMMIT_REF,
		IssueID:   issue.ID,
		CommitSHA: commitSHA,
	})
	if err != nil {
		return fmt.Errorf("check reference comment: %v", err)
	} else if has {
		return nil
	}

	_, err = CreateComment(&CreateCommentOptions{
		Type:      COMMENT_TYPE_COMMIT_REF,
		Doer:      doer,
		Repo:      repo,
		Issue:     issue,
		CommitSHA: commitSHA,
		Content:   content,
	})
	return err
}

// GetCommentByID returns the comment by given ID.
func GetCommentByID(id int64) (*Comment, error) {
	c := new(Comment)
	has, err := x.Id(id).Get(c)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrCommentNotExist{id, 0}
	}
	return c, nil
}

func getCommentsByIssueIDSince(e Engine, issueID, since int64) ([]*Comment, error) {
	comments := make([]*Comment, 0, 10)
	sess := e.Where("issue_id = ?", issueID).Asc("created_unix")
	if since > 0 {
		sess.And("updated_unix >= ?", since)
	}
	return comments, sess.Find(&comments)
}

func getCommentsByIssueID(e Engine, issueID int64) ([]*Comment, error) {
	return getCommentsByIssueIDSince(e, issueID, -1)
}

// GetCommentsByIssueID returns all comments of an issue.
func GetCommentsByIssueID(issueID int64) ([]*Comment, error) {
	return getCommentsByIssueID(x, issueID)
}

// GetCommentsByIssueID returns a list of comments of an issue since a given time point.
func GetCommentsByIssueIDSince(issueID, since int64) ([]*Comment, error) {
	return getCommentsByIssueIDSince(x, issueID, since)
}

// UpdateComment updates information of comment.
func UpdateComment(c *Comment) error {
	_, err := x.Id(c.ID).AllCols().Update(c)
	return err
}

// DeleteCommentByID deletes the comment by given ID.
func DeleteCommentByID(id int64) error {
	comment, err := GetCommentByID(id)
	if err != nil {
		if IsErrCommentNotExist(err) {
			return nil
		}
		return err
	}

	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return err
	}

	if _, err = sess.Id(comment.ID).Delete(new(Comment)); err != nil {
		return err
	}

	if comment.Type == COMMENT_TYPE_COMMENT {
		if _, err = sess.Exec("UPDATE `issue` SET num_comments = num_comments - 1 WHERE id = ?", comment.IssueID); err != nil {
			return err
		}
	}

	return sess.Commit()
}
