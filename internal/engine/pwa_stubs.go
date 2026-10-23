package engine

// pwa_stubs.go: Notifications and Web Push are not part of the agent browser. There is no notification surface and no
// push service, so the host functions behind Notification and PushManager answer the way a browser without them does:
// the permission is denied (see pwa.go), show and subscribe fail, lists are empty.

func (h *pwaHost) notifyFns(f map[string]hostFn) {
	f["pwaNotifyShow"] = func(a []any) (any, error) {
		h.reply(argI(a, 0), nil, &swError{"NotAllowedError", "Notifications are not available."})
		return nil, nil
	}
	f["pwaNotifyClose"] = func(a []any) (any, error) { return nil, nil }
	f["pwaNotifyList"] = func(a []any) (any, error) { return []any{}, nil }
}

func (h *pwaHost) pushFns(f map[string]hostFn) {
	f["pwaPushGet"] = func(a []any) (any, error) { return nil, nil }
	f["pwaPushAvailable"] = func(a []any) (any, error) { return false, nil }
	f["pwaPushSubscribe"] = func(a []any) (any, error) {
		h.reply(argI(a, 0), nil, &swError{"AbortError", "Registration failed - push service not available"})
		return nil, nil
	}
	f["pwaPushUnsubscribe"] = func(a []any) (any, error) {
		h.reply(argI(a, 0), false, nil)
		return nil, nil
	}
}
