package compaction

// Trigger is a predicate over message-index metrics used to start or stop
// compaction.
type Trigger func(*MessageIndex) bool

// Always returns a trigger that always fires.
func Always() Trigger { return func(*MessageIndex) bool { return true } }

// Never returns a trigger that never fires.
func Never() Trigger { return func(*MessageIndex) bool { return false } }

// TokensBelow returns a trigger that fires when included tokens < maxTokens.
func TokensBelow(maxTokens int) Trigger {
	return func(idx *MessageIndex) bool { return idx.IncludedTokenCount() < maxTokens }
}

// TokensExceed returns a trigger that fires when included tokens > maxTokens.
func TokensExceed(maxTokens int) Trigger {
	return func(idx *MessageIndex) bool { return idx.IncludedTokenCount() > maxTokens }
}

// MessagesExceed returns a trigger when included message count > maxMessages.
func MessagesExceed(maxMessages int) Trigger {
	return func(idx *MessageIndex) bool { return idx.IncludedMessageCount() > maxMessages }
}

// TurnsExceed returns a trigger when included user turns > maxTurns.
func TurnsExceed(maxTurns int) Trigger {
	return func(idx *MessageIndex) bool { return idx.IncludedTurnCount() > maxTurns }
}

// GroupsExceed returns a trigger when included group count > maxGroups.
func GroupsExceed(maxGroups int) Trigger {
	return func(idx *MessageIndex) bool { return idx.IncludedGroupCount() > maxGroups }
}

// All returns a compound trigger that fires only when every trigger fires.
func All(triggers ...Trigger) Trigger {
	return func(idx *MessageIndex) bool {
		for _, t := range triggers {
			if t == nil || !t(idx) {
				return false
			}
		}
		return true
	}
}

// Any returns a compound trigger that fires when at least one trigger fires.
func Any(triggers ...Trigger) Trigger {
	return func(idx *MessageIndex) bool {
		for _, t := range triggers {
			if t != nil && t(idx) {
				return true
			}
		}
		return false
	}
}
