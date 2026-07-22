package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type announcementRepoStub struct {
	item *Announcement
}

type announcementUserRepoStub struct {
	UserRepository
	users []User
	page  *pagination.PaginationResult
}

func (s *announcementUserRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, UserListFilters) ([]User, *pagination.PaginationResult, error) {
	return s.users, s.page, nil
}

type announcementReadRepoStub struct {
	AnnouncementReadRepository
	reads map[int64]time.Time
}

func (s *announcementReadRepoStub) GetReadMapByUsers(context.Context, int64, []int64) (map[int64]time.Time, error) {
	return s.reads, nil
}

type announcementAudienceRepoStub struct {
	groupsByUser ActiveGroupIDsByUser
	calls        int
	userIDs      []int64
}

func (s *announcementAudienceRepoStub) GetActiveGroupIDsByUserIDs(_ context.Context, userIDs []int64) (ActiveGroupIDsByUser, error) {
	s.calls++
	s.userIDs = append([]int64(nil), userIDs...)
	return s.groupsByUser, nil
}

func (s *announcementRepoStub) Create(_ context.Context, a *Announcement) error {
	s.item = a
	return nil
}

func (s *announcementRepoStub) GetByID(_ context.Context, _ int64) (*Announcement, error) {
	if s.item == nil {
		return nil, ErrAnnouncementNotFound
	}
	return s.item, nil
}

func (s *announcementRepoStub) Update(_ context.Context, a *Announcement) error {
	s.item = a
	return nil
}

func (*announcementRepoStub) Delete(context.Context, int64) error {
	return nil
}

func (*announcementRepoStub) List(context.Context, pagination.PaginationParams, AnnouncementListFilters) ([]Announcement, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (*announcementRepoStub) ListActive(context.Context, time.Time) ([]Announcement, error) {
	return nil, nil
}

func TestAnnouncementServiceCreateRejectsEqualStartEndTimes(t *testing.T) {
	repo := &announcementRepoStub{}
	svc := NewAnnouncementService(repo, nil, nil, nil, nil)
	now := time.Unix(1776790020, 0)

	_, err := svc.Create(context.Background(), &CreateAnnouncementInput{
		Title:      "公告",
		Content:    "内容",
		Status:     AnnouncementStatusActive,
		NotifyMode: AnnouncementNotifyModePopup,
		StartsAt:   &now,
		EndsAt:     &now,
	})
	require.ErrorIs(t, err, ErrAnnouncementInvalidSchedule)
}

func TestAnnouncementServiceUpdateRejectsEqualStartEndTimes(t *testing.T) {
	repo := &announcementRepoStub{
		item: &Announcement{
			ID:         1,
			Title:      "公告",
			Content:    "内容",
			Status:     AnnouncementStatusActive,
			NotifyMode: AnnouncementNotifyModePopup,
		},
	}
	svc := NewAnnouncementService(repo, nil, nil, nil, nil)
	now := time.Unix(1776790020, 0)
	startsAt := &now
	endsAt := &now

	_, err := svc.Update(context.Background(), 1, &UpdateAnnouncementInput{
		StartsAt: &startsAt,
		EndsAt:   &endsAt,
	})
	require.ErrorIs(t, err, ErrAnnouncementInvalidSchedule)
}

func TestAnnouncementServiceListUserReadStatusLoadsAudienceInOneBatch(t *testing.T) {
	readAt := time.Unix(1776790020, 0)
	repo := &announcementRepoStub{item: &Announcement{
		ID: 1,
		Targeting: AnnouncementTargeting{AnyOf: []AnnouncementConditionGroup{{AllOf: []AnnouncementCondition{{
			Type:     AnnouncementConditionTypeSubscription,
			Operator: AnnouncementOperatorIn,
			GroupIDs: []int64{10},
		}}}}},
	}}
	userRepo := &announcementUserRepoStub{
		users: []User{
			{ID: 1, Email: "eligible@example.com", Balance: 5},
			{ID: 2, Email: "other@example.com", Balance: 7},
		},
		page: &pagination.PaginationResult{Total: 2, Page: 1, PageSize: 20, Pages: 1},
	}
	readRepo := &announcementReadRepoStub{reads: map[int64]time.Time{1: readAt}}
	audienceRepo := &announcementAudienceRepoStub{groupsByUser: ActiveGroupIDsByUser{
		1: map[int64]struct{}{10: {}},
		2: map[int64]struct{}{20: {}},
	}}
	svc := NewAnnouncementService(repo, readRepo, userRepo, nil, audienceRepo)

	statuses, page, err := svc.ListUserReadStatus(context.Background(), 1, pagination.PaginationParams{Page: 1, PageSize: 20}, "")

	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	require.Equal(t, 1, audienceRepo.calls)
	require.Equal(t, []int64{1, 2}, audienceRepo.userIDs)
	require.Len(t, statuses, 2)
	require.True(t, statuses[0].Eligible)
	require.Equal(t, &readAt, statuses[0].ReadAt)
	require.False(t, statuses[1].Eligible)
	require.Nil(t, statuses[1].ReadAt)
}
