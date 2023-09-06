package j2g

import (
	"fmt"
	"regexp"
	"sync"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/onpremise"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	gitlab "github.com/xanzy/go-gitlab"
	"gitlab.com/infograb/team/devops/toy/j2lab/internal/config"
	"gitlab.com/infograb/team/devops/toy/j2lab/internal/gitlabx"
	"gitlab.com/infograb/team/devops/toy/j2lab/internal/utils"
	"golang.org/x/sync/errgroup"
)

func ConvertJiraIssueToGitLabEpic(gl *gitlab.Client, jr *jira.Client, jiraIssue *jira.Issue, userMap UserMap) (*gitlab.Epic, error) {
	log := logrus.WithField("jiraEpic", jiraIssue.Key)
	var g errgroup.Group
	g.SetLimit(5)
	mutex := sync.RWMutex{}

	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "Error getting config")
	}

	gid := cfg.Project.GitLab.Epic

	labels, err := convertJiraToGitLabLabels(gl, jr, gid, jiraIssue, true)
	if err != nil {
		return nil, errors.Wrap(err, "Error converting Jira labels to GitLab labels")
	}

	gitlabCreateEpicOptions := gitlabx.CreateEpicOptions{
		Title:        gitlab.String(jiraIssue.Fields.Summary),
		Color:        utils.RandomColor(),
		CreatedAt:    (*time.Time)(&jiraIssue.Fields.Created),
		Labels:       labels,
		DueDateFixed: (*gitlab.ISOTime)(&jiraIssue.Fields.Duedate),
	}

	//* Attachment for Description and Comments
	//! Epic Attachment는 API가 없는 관계로 우회한다.
	// 1. cfg.Project.GitLab.Issue 프로젝트에 attachement를 붙인다.
	// 2. 결과 markdown을 절대 경로로 바꾼 후 epic description에 붙인다
	pid := cfg.Project.GitLab.Issue
	usedAttachment := make(map[string]bool)

	attachments := make(map[string]*Attachment) // ID -> Markdown
	for _, jiraAttachment := range jiraIssue.Fields.Attachments {
		g.Go(func(jiraAttachment *jira.Attachment) func() error {
			return func() error {
				attachment, err := convertJiraAttachmentToMarkdown(gl, jr, pid, jiraAttachment)
				if err != nil {
					return errors.Wrap(err, "Error converting Jira attachment to GitLab attachment")
				}

				regexp := regexp.MustCompile(`!\[(.+)\]\((.+)\)`)
				matches := regexp.FindStringSubmatch(attachment.Markdown)

				if len(matches) != 3 {
					return errors.Wrap(err, "Error parsing markdown")
				}

				alt := matches[1]
				url := matches[2]

				absUrl := fmt.Sprintf("%s/%s/%s", cfg.GitLab.Host, cfg.Project.GitLab.Issue, url)

				mutex.Lock()
				attachments[jiraAttachment.ID] = &Attachment{
					Markdown:  fmt.Sprintf("![%s](%s)", alt, absUrl),
					Name:      attachment.Name,
					CreatedAt: attachment.CreatedAt,
				}
				mutex.Unlock()
				log.Debugf("Converted attachment: %s to %s", jiraAttachment.ID, attachment.Markdown)
				return nil
			}
		}(jiraAttachment))
	}

	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, "Error converting Jira attachment to GitLab attachment")
	}

	//* Description -> Description
	description, err := formatDescription(jiraIssue, userMap, attachments, true)
	if err != nil {
		return nil, errors.Wrap(err, "Error formatting description")
	}
	gitlabCreateEpicOptions.Description = description

	//* StartDate
	if cfg.Project.Jira.CustomField.EpicStartDate != "" {
		startDateStr, ok := jiraIssue.Fields.Unknowns[cfg.Project.Jira.CustomField.EpicStartDate].(string)
		if ok {
			startDate, err := time.Parse("2006-01-02", startDateStr)
			if err != nil {
				return nil, errors.Wrap(err, "Error parsing time")
			}

			gitlabCreateEpicOptions.StartDateIsFixed = gitlab.Bool(true)
			gitlabCreateEpicOptions.StartDateFixed = (*gitlab.ISOTime)(&startDate)
		} else {
			log.Warnf("Unable to convert epic start date from Jira issue %s to GitLab start date", jiraIssue.Key)
		}
	}

	//* DueDate
	if jiraIssue.Fields.Duedate != (jira.Date{}) {
		gitlabCreateEpicOptions.DueDateIsFixed = gitlab.Bool(true)
		gitlabCreateEpicOptions.DueDateFixed = (*gitlab.ISOTime)(&jiraIssue.Fields.Duedate)
	}

	//* 에픽을 생성합니다.
	gitlabEpic, _, err := gitlabx.CreateEpic(gl, cfg.Project.GitLab.Epic, &gitlabCreateEpicOptions)
	if err != nil {
		return nil, errors.Wrap(err, "Error creating GitLab epic")
	}
	log.Debugf("Created GitLab epic: %d from Jira issue: %s", gitlabEpic.IID, jiraIssue.Key)

	//* Comment -> Comment
	for _, jiraComment := range jiraIssue.Fields.Comments.Comments {
		g.Go(func(jiraComment *jira.Comment) func() error {
			return func() error {
				body, _, err := formatNote(jiraIssue.Key, jiraComment, userMap, attachments, true)
				if err != nil {
					return errors.Wrap(err, "Error formatting comment")
				}

				createEpicNoteOptions := gitlab.CreateEpicNoteOptions{
					Body: body,
				}

				_, _, err = gl.Notes.CreateEpicNote(gid, gitlabEpic.ID, &createEpicNoteOptions)
				if err != nil {
					return errors.Wrap(err, "Error creating note")
				}
				return nil
			}
		}(jiraComment))
	}

	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("Error creating GitLab comment with gid %s, epic ID %d", gid, gitlabEpic.ID))
	}

	//* Reamin Attachment -> Comment
	for id, markdown := range attachments {
		if used, ok := usedAttachment[id]; ok || used {
			continue
		}

		g.Go(func(markdown *Attachment) func() error {
			return func() error {
				_, _, err = gl.Notes.CreateEpicNote(gid, gitlabEpic.ID, &gitlab.CreateEpicNoteOptions{
					Body: &markdown.Markdown,
				})
				if err != nil {
					return errors.Wrap(err, "Error creating note")
				}
				return nil
			}
		}(markdown))
	}

	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, "Error creating GitLab issue")
	}

	//* Resolution -> Close issue (CloseAt)
	if jiraIssue.Fields.Resolution != nil {
		gl.Epics.UpdateEpic(gid, gitlabEpic.IID, &gitlab.UpdateEpicOptions{
			StateEvent: gitlab.String("close"),
		})
		log.Debugf("Closed GitLab epic: %d", gitlabEpic.IID)
	}

	return gitlabEpic, nil
}
