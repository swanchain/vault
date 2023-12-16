package vault

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/sdk/helper/base62"
	"github.com/hashicorp/vault/sdk/logical"

	log "github.com/hashicorp/go-hclog"

	"github.com/robfig/cron/v3"

	"github.com/hashicorp/vault/sdk/queue"
)

const parseOptions = cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow

var parser = cron.NewParser(parseOptions)

type RotationManager struct {
	logger log.Logger
	mu     sync.Mutex

	queue queue.PriorityQueue
	done  chan struct{}

	router   *Router
	backends func() *[]MountEntry // list of logical and auth backends, remember to call RUnlock
}

// rotationEntry is used to structure the values the expiration
// manager stores. This is used to handle renew and revocation.
type rotationEntry struct {
	RotationID     string                  `json:"rotation_id"`
	Path           string                  `json:"path"`
	Data           map[string]interface{}  `json:"data"`
	RootCredential *logical.RootCredential `json:"static_secret"`
	IssueTime      time.Time               `json:"issue_time"`
	ExpireTime     time.Time               `json:"expire_time"`

	namespace *namespace.Namespace
}

func (rm *RotationManager) Start() error {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		rm.logger.Info("started ticker")
		for {
			rm.mu.Lock()
			select {
			case <-rm.done:
				return
			case t := <-ticker.C:
				rm.logger.Info("time", "time", t.Format(time.RFC3339))
				err := rm.CheckQueue()
				if err != nil {
					rm.logger.Error("check queue error", "err", err)
				}
			}
		}
	}()
	return nil
}

func (rm *RotationManager) CheckQueue() error {
	// loop runs forever, so break whenever you get to the first credential that doesn't need updating
	for {
		now := time.Now()
		i, err := rm.queue.Pop()
		if err != nil {
			rm.logger.Info("queue empty")
			return nil
		}

		var re rotationEntry
		entry, ok := i.Value.(rotationEntry)
		if !ok {
			return fmt.Errorf("error parsing rotation entry from queue")
		}

		re = entry

		if i.Priority > now.Unix() {
			err := rm.queue.Push(i)
			if err != nil {
				// this is pretty bad because we have no real way to fix it and save the item, but the Push operation only
				// errors on malformed items, which shouldn't be possible here
				return err
			}
			break // this item is not ripe yet, which means all later items are also unripe, so exit the check loop
		}

		// TODO should we push the credential back into the queue if it is not in the rotation window?
		// if not in window, do we check the next credential?
		if !logical.DefaultScheduler.IsInsideRotationWindow(re.RootCredential.Schedule, time.Now()) {
			err := rm.queue.Push(i)
			if err != nil {
				// this is pretty bad because we have no real way to fix it and save the item, but the Push operation only
				// errors on malformed items, which shouldn't be possible here
				return err
			}
			break
		}

		// do rotation
		req := &logical.Request{
			Operation: logical.RotationOperation,
			Path:      "path",
		}
		_, err = rm.router.Route(context.Background(), req)
		if errors.Is(err, logical.ErrUnsupportedOperation) {
			rm.logger.Info("unsupported")
			continue
		} else if err != nil {
			// requeue with backoff
			rm.logger.Info("other rotate error", "err", err)
			// TODO: We can either check the window here, or let the priority check above handle it
			i.Priority = i.Priority + 10
		}

		// success
		issueTime := time.Now()
		newEntry := &rotationEntry{
			RotationID:     re.RotationID,
			Path:           re.Path,
			Data:           re.Data,
			RootCredential: re.RootCredential,
			IssueTime:      issueTime,
			// expires the next time the schedule is activated from the issue time
			ExpireTime: re.RootCredential.Schedule.Schedule.Next(issueTime),
			namespace:  re.namespace,
		}

		// lock and populate the queue
		rm.mu.Lock()
		defer rm.mu.Unlock()

		item := &queue.Item{
			Key:      newEntry.RotationID,
			Value:    newEntry,
			Priority: newEntry.ExpireTime.Unix(),
		}

		rm.logger.Debug("Pushing item into credential queue")

		if err := rm.queue.Push(item); err != nil {
			// TODO handle error
			rm.logger.Debug("Error pushing item into credential queue")
			return err
		}
		i.Priority = re.RootCredential.Schedule.Schedule.Next(time.Now()).Unix()
		err = rm.queue.Push(i)
		if err != nil {
			// again, this is bad because we can't really fix the item, but it also shouldn't happen because the item was good before
			return err
		}
	}

	return nil
}

// Rotate is used to rotate a credential named by the given rotationID
func (r *RotationManager) Rotate(ctx context.Context, rotationID string) error {
	r.logger.Debug("RotationManager.Rotate called")
	// TODO: rotate

	return nil
}

// Register takes a request and response with an associated StaticSecret. The
// secret gets assigned a RotationID and the management of the rotation is
// assumed by the rotation manager.
func (r *RotationManager) Register(ctx context.Context, req *logical.Request, resp *logical.Response) (id string, retErr error) {
	// Ignore if there is no static secret
	if resp == nil || resp.RootCredential == nil {
		return "", nil
	}

	// TODO: Check if we need to validate the root credential

	// Create a rotation entry. We use TokenLength because that is what is used
	// by ExpirationManager
	rotationRand, err := base62.Random(TokenLength)
	if err != nil {
		return "", err
	}

	ns, err := namespace.FromContext(ctx)
	if err != nil {
		return "", err
	}

	rotationID := path.Join(req.Path, rotationRand)

	if ns.ID != namespace.RootNamespaceID {
		rotationID = fmt.Sprintf("%s.%s", rotationID, ns.ID)
	}

	issueTime := time.Now()
	re := &rotationEntry{
		RotationID:     rotationID,
		Path:           req.Path,
		Data:           resp.Data,
		RootCredential: resp.RootCredential,
		IssueTime:      issueTime,
		// expires the next time the schedule is activated from the issue time
		ExpireTime: resp.RootCredential.Schedule.Schedule.Next(issueTime),
		namespace:  ns,
	}

	// lock and populate the queue
	r.mu.Lock()
	defer r.mu.Unlock()

	// @TODO for different cases, update rotation entry if it is already in queue
	// for now, assuming it is a fresh root credential and the schedule is not being updated
	item := &queue.Item{
		Key:      re.RotationID,
		Value:    re,
		Priority: re.ExpireTime.Unix(),
	}

	r.logger.Debug("Pushing item into credential queue")

	if err := r.queue.Push(item); err != nil {
		// TODO handle error
		r.logger.Debug("Error pushing item into credential queue")
		return "", err
	}

	return re.RotationID, nil
}

// A RotationSchedule is a way to store the requested rotation schedule of a credential
//type RotationSchedule struct {
//	s cron.Schedule
//}
//
//func ParseSchedule(s string) (*RotationSchedule, error) {
//	c, err := parser.Parse(s)
//	if err != nil {
//		return nil, err
//	}
//
//	return &RotationSchedule{
//		s: c,
//	}, nil
//}

func (c *Core) startRotation() error {
	logger := c.baseLogger.Named("rotation")
	c.AddLogger(logger)
	c.rotationManager = &RotationManager{
		logger: logger,
		queue:  queue.PriorityQueue{},
		done:   make(chan struct{}),
	}
	err := c.rotationManager.Start()
	if err != nil {
		return err
	}
	return nil
}
