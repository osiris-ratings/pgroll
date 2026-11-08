// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"slices"
)

// Coordinator is responsible for executing a series of database actions in a specific orderedActions.
// It ensures that each action is executed only once, even if it is added multiple times.
type Coordinator struct {
	actions        map[string]DBAction
	orderedActions []string
}

func NewCoordinator(actions []DBAction) *Coordinator {
	actionsMap := make(map[string]DBAction, len(actions))
	orderedActions := make([]string, 0)
	for _, action := range actions {
		if act, exists := actionsMap[action.ID()]; exists {
			idx := slices.Index(orderedActions, act.ID())
			orderedActions = slices.Delete(orderedActions, idx, idx+1)
		} else {
			actionsMap[action.ID()] = action
		}
		orderedActions = append(orderedActions, action.ID())
	}
	return &Coordinator{
		actions:        actionsMap,
		orderedActions: orderedActions,
	}
}

// Execute runs all actions in the order they were added to the coordinator.
func (c *Coordinator) Execute(ctx context.Context) error {
	return c.ExecuteWithRecovery(ctx, nil)
}

// ExecuteWithRecovery is Execute with a second chance: an action that fails
// is handed to recover with its error, and recover either runs it another way
// (returning nil) or returns the error to fail with. A nil recover is plain
// Execute.
func (c *Coordinator) ExecuteWithRecovery(ctx context.Context, recover func(context.Context, DBAction, error) error) error {
	for _, id := range c.orderedActions {
		action, exists := c.actions[id]
		if !exists {
			return fmt.Errorf("action %s not found", id)
		}
		err := action.Execute(ctx)
		if err != nil && recover != nil {
			err = recover(ctx, action, err)
		}
		if err != nil {
			return fmt.Errorf("failed to execute action %s: %w", id, err)
		}
	}
	return nil
}
