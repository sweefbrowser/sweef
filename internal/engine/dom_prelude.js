/*
 * Sweef DOM prelude — the browser environment for page scripts.
 *
 * Runs inside QuickJS (ES2023, no DOM). The Go tree is the single source of
 * truth; everything here reaches it through the host object `__sw` described
 * in jsbridge.md. Plain ES2020, no modules, installs everything on globalThis.
 *
 * Structure (search for "// ====" to jump between sections):
 *   1. Utilities            small helpers, property definers, host shims
 *   2. Wrapper cache        wrappers Map id -> object, wrap()/unwrap(), classes by tag
 *   3. Events               Event + subclasses, EventTarget, dispatch path, on* props
 *   4. Collections          NodeList, HTMLCollection, DOMTokenList, NamedNodeMap,
 *                           CSSStyleDeclaration, dataset proxy
 *   5. Nodes                Node, Element, HTMLElement + subclasses, SVGElement,
 *                           Text, Comment, DocumentType, DocumentFragment, Document
 *   6. Reflected attributes generated accessors from a table
 *   7. Observers            MutationObserver (microtask delivery), Intersection/Resize
 *   8. Timers               heap owned by JS, pumped by Go via __swRunTimers
 *   9. Platform             location/history/navigator/screen/storage/console/
 *                           fetch/XHR/URL/encoding/crypto/performance/misc stubs
 *  10. Lifecycle & exports  __swDispatch, __swDispatchKey, __swMouse, __swTouch,
 *                           __swDrag, __swWheel, __swSetValueFromGo, __swRunTimers,
 *                           __swFlushMutationObservers, __swFireLifecycle,
 *                           __swSetDocument, __swDefaultAction, __swSerialize
 */
(function (global) {
  'use strict';
  if (global.__swPreludeInstalled) return;
  Object.defineProperty(global, '__swPreludeInstalled', { value: true });

  // ==== 1. Utilities ======================================================
  var sw = global.__sw;
  if (!sw) throw new Error('dom_prelude: host object __sw missing');

  var hasOwn = Object.prototype.hasOwnProperty;
  var slice = Array.prototype.slice;

  function host(name) { return typeof sw[name] === 'function'; }
  // optional host calls (not all hosts implement telemetry/geometry)
  function hostBox(id) {
    if (host('box')) { var b = sw.box(id); if (b) return b; }
    return { x: 0, y: 0, width: 0, height: 0 };
  }
  // where the viewport is scrolled to ([x, y] in CSS px): geometry the page sees is relative to the viewport
  function scrollPos() { if (host('scrollPos')) { var p = sw.scrollPos(); if (p) return p; } return [0, 0]; }
  function hostMutation(op, id) { if (host('mutation')) { try { sw.mutation(op, id); } catch (e) { /* ignore */ } } }
  function hostLog(level, text, loc) { try { sw.log(level, text, loc || ''); } catch (e) { /* ignore */ } }
  // where a console call came from: "line:col" of the caller (the frame after this function and the console method),
  // for Developer Tools; the host adds the name of the script whose top level is running
  function callerLoc() {
    try {
      var f = String(new Error().stack || '').split('\n')[2] || '', m = /<eval>:(\d+:\d+)\)?\s*$/.exec(f);
      return m ? m[1] : '';
    } catch (e) { /* ignore */ }
    return '';
  }

  function def(obj, name, desc) {
    desc.configurable = true;
    if (!('get' in desc) && !('set' in desc)) desc.writable = true;
    if (desc.enumerable === undefined) desc.enumerable = !('get' in desc) ? false : true;
    Object.defineProperty(obj, name, desc);
  }
  function method(proto, name, fn) { def(proto, name, { value: fn, enumerable: false }); }
  function methods(proto, map) { for (var k in map) if (hasOwn.call(map, k)) method(proto, k, map[k]); }
  function getter(proto, name, get, set) { def(proto, name, { get: get, set: set, enumerable: true }); }
  function getters(proto, map) { for (var k in map) if (hasOwn.call(map, k)) def(proto, k, { get: map[k].get, set: map[k].set, enumerable: true }); }
  function constant(obj, name, v) { Object.defineProperty(obj, name, { value: v, enumerable: true }); }
  function toStr(v) { return v === undefined ? 'undefined' : v === null ? 'null' : String(v); }
  // cssStr escapes a value for a double-quoted CSS string in a selector: backslash first, then the quote,
  // and line breaks as hex escapes (a raw newline ends a CSS string).
  function cssStr(v) { return String(v).replace(/[\\"]/g, '\\$&').replace(/\n/g, '\\a ').replace(/\r/g, '\\d ').replace(/\f/g, '\\c '); }
  function illegal() { throw new TypeError('Illegal constructor'); }
  function setTag(cls, name) { def(cls.prototype, Symbol.toStringTag, { value: name, enumerable: false }); }
  function extend(sub, sup) { Object.setPrototypeOf(sub.prototype, sup.prototype); Object.setPrototypeOf(sub, sup); }
  function makeClass(name, sup, ctor) {
    var C = ctor || function () { illegal(); };
    if (sup) extend(C, sup);
    setTag(C, name);
    def(C, 'name', { value: name, enumerable: false, writable: false });
    global[name] = C;
    return C;
  }
  function DOMException(message, name) {
    var e = new Error(message || '');
    e.name = name || 'Error';
    var codes = { IndexSizeError: 1, HierarchyRequestError: 3, NotFoundError: 8, NotSupportedError: 9, InvalidStateError: 11, SyntaxError: 12, InvalidCharacterError: 5, AbortError: 20, SecurityError: 18, QuotaExceededError: 22 };
    e.code = codes[e.name] || 0;
    Object.setPrototypeOf(e, DOMException.prototype);
    return e;
  }
  DOMException.prototype = Object.create(Error.prototype, { constructor: { value: DOMException, writable: true, configurable: true } });
  DOMException.prototype.name = 'Error';
  global.DOMException = DOMException;

  // ==== 2. Wrapper cache ===================================================
  var wrappers = new Map();      // id -> wrapper object (stable: expandos persist)
  var docId = 0;                 // id of the Document node
  var documentWrapper = null;    // the `document` global
  var ID = Symbol('sweefId');    // wrapper[ID] -> node id

  function idOf(node) { return node && node[ID] !== undefined ? node[ID] : 0; }
  function isNodeObj(v) { return !!v && typeof v === 'object' && v[ID] !== undefined; }

  var SVG_TAGS = { svg: 1, path: 1, g: 1, circle: 1, rect: 1, line: 1, polyline: 1, polygon: 1, ellipse: 1, text: 1, tspan: 1, defs: 1, use: 1, symbol: 1, lineargradient: 1, radialgradient: 1, stop: 1, clippath: 1, mask: 1, pattern: 1, image: 1, foreignobject: 1, marker: 1, filter: 1, fegaussianblur: 1, feoffset: 1, feblend: 1, fecolormatrix: 1, animate: 1, animatetransform: 1, desc: 1, title: 0, style: 0, script: 0 };
  var TAG_CLASS = {}; // lower tag -> constructor (filled after classes exist)

  function classFor(id, type, tag) {
    if (type === 1) {
      var C = TAG_CLASS[tag];
      if (C) return C;
      if (SVG_TAGS[tag] === 1) return global.SVGElement;
      if (tag.indexOf('-') > 0) return global.HTMLElement; // custom elements upgraded separately
      return global.HTMLUnknownElement || global.HTMLElement;
    }
    if (type === 3) return global.Text;
    if (type === 8) return global.Comment;
    if (type === 9) return global.Document;
    if (type === 10) return global.DocumentType;
    return global.Node;
  }

  function makeWrapper(id, C) {
    var w = Object.create(C.prototype);
    Object.defineProperty(w, ID, { value: id });
    wrappers.set(id, w);
    return w;
  }

  function wrap(id) {
    if (!id) return null;
    var w = wrappers.get(id);
    if (w) return w;
    var type = sw.nodeType(id);
    if (!type) return null;
    var tag = type === 1 ? sw.tagName(id) : '';
    w = makeWrapper(id, classFor(id, type, tag));
    if (type === 1) {
      if (tag === 'template') sawTemplate = true;
      else if (tag.indexOf('-') > 0) maybeUpgrade(w, tag);
    }
    return w;
  }
  // Developer Tools edits a node the way page script would, so MutationObservers and custom elements see the change
  def(global, '__swDevNode', { value: function (id) { return wrap(+id); }, enumerable: false });
  function wrapAll(ids) { var out = []; if (ids) for (var i = 0; i < ids.length; i++) { var w = wrap(ids[i]); if (w) out.push(w); } return out; }
  function requireNode(v, what) {
    if (!isNodeObj(v)) throw new TypeError("Failed to execute '" + what + "': parameter 1 is not of type 'Node'.");
    return v;
  }

  // custom elements (filled by CustomElementRegistry later)
  var ceDefinitions = new Map(); // name -> {ctor, observed}
  var ceUpgrading = null;
  // Template contents are inert: their elements belong to a document without a custom element registry, so a script that
  // walks template.content (Polymer parses its templates that way) must not construct them, with bindings such as
  // shown-items="[[shownItems]]" still in their attributes. The contents sit in the host tree under the <template>
  // (until .content moves them into a fragment), so check both homes.
  // Elements made by an inert document (the template contents owner document, document.implementation.createHTMLDocument)
  // are not upgraded either, until they are inserted into a connected tree.
  var INERT = Symbol('inertFragment'), INERT_EL = Symbol('inertElement');
  var sawTemplate = false, templateDocument = null, noUpgrade = false;
  function inTemplateContent(w) {
    if (w[INERT] || w[FRAG] && w[FRAG][INERT]) return true;
    for (var p = sw.parent(w[ID]); p; p = sw.parent(p)) {
      if (sw.tagName(p) === 'template') return true;
      var pw = wrappers.get(p);
      if (pw && pw[FRAG] && pw[FRAG][INERT]) return true;
    }
    return false;
  }
  function maybeUpgrade(w, tag) {
    var d = ceDefinitions.get(tag);
    if (!d || noUpgrade || w[INERT_EL] || Object.getPrototypeOf(w) === d.ctor.prototype || inTemplateContent(w)) return;
    Object.setPrototypeOf(w, d.ctor.prototype);
    var prev = ceUpgrading; ceUpgrading = w;
    try { new d.ctor(); } catch (e) { reportError(e); } finally { ceUpgrading = prev; }
    if (sw.attached(w[ID]) && typeof w.connectedCallback === 'function') { try { w.connectedCallback(); } catch (e) { reportError(e); } }
  }
  function ceConnected(w) {
    if (!w || w.nodeType !== 1) return;
    if (w[INERT_EL]) delete w[INERT_EL];
    var d = ceDefinitions.get(w.localName);
    if (d && Object.getPrototypeOf(w) !== d.ctor.prototype) maybeUpgrade(w, w.localName); // adopted out of a template: upgrade now (also runs connectedCallback)
    else if (d && typeof w.connectedCallback === 'function') { try { w.connectedCallback(); } catch (e) { reportError(e); } }
    var kids = sw.children(w[ID]);
    for (var i = 0; i < kids.length; i++) { var k = wrappers.get(kids[i]); if (k) ceConnected(k); }
  }
  function ceDisconnected(w) {
    if (!w || w.nodeType !== 1) return;
    if (popoverOpen(w)) hidePopoverImpl(w, false, false); // a popover that leaves the document closes without events
    if (typeof w.disconnectedCallback === 'function' && ceDefinitions.has(w.localName)) { try { w.disconnectedCallback(); } catch (e) { reportError(e); } }
    var kids = sw.children(w[ID]);
    for (var i = 0; i < kids.length; i++) { var k = wrappers.get(kids[i]); if (k) ceDisconnected(k); }
  }

  // ==== 3. Events ==========================================================
  var wheelNP = false; // a non-passive wheel listener has been added
  var LISTENERS = Symbol('listeners'); // target[LISTENERS] = { type: [ {fn, capture, once, passive, removed} ] }

  function Event(type, init) {
    if (!(this instanceof Event)) throw new TypeError("Constructor Event requires 'new'");
    if (arguments.length === 0) throw new TypeError("Failed to construct 'Event': 1 argument required");
    initEventFields(this, String(type), init || {});
  }
  function initEventFields(ev, type, init) {
    ev.type = type;
    ev.bubbles = !!init.bubbles;
    ev.cancelable = !!init.cancelable;
    ev.composed = !!init.composed;
    ev.target = null; ev.currentTarget = null; ev.srcElement = null;
    ev.eventPhase = 0;
    ev.defaultPrevented = false;
    ev.isTrusted = false;
    ev.timeStamp = nowMs();
    ev.returnValue = true;
    ev.cancelBubble = false;
    ev._stop = false; ev._stopNow = false; ev._path = null; ev._dispatching = false;
  }
  Event.prototype = {
    constructor: Event,
    get NONE() { return 0; }, get CAPTURING_PHASE() { return 1; }, get AT_TARGET() { return 2; }, get BUBBLING_PHASE() { return 3; },
    stopPropagation: function () { this._stop = true; this.cancelBubble = true; },
    stopImmediatePropagation: function () { this._stop = true; this._stopNow = true; this.cancelBubble = true; },
    preventDefault: function () { if (this.cancelable && !this._passive) { this.defaultPrevented = true; this.returnValue = false; } },
    composedPath: function () { return this._path ? this._path.slice() : []; },
    initEvent: function (type, bubbles, cancelable) { if (this._dispatching) return; this.type = String(type); this.bubbles = !!bubbles; this.cancelable = !!cancelable; this.defaultPrevented = false; }
  };
  constant(Event, 'NONE', 0); constant(Event, 'CAPTURING_PHASE', 1); constant(Event, 'AT_TARGET', 2); constant(Event, 'BUBBLING_PHASE', 3);
  setTag(Event, 'Event');
  global.Event = Event;

  // Event subclasses generated from a table: name -> [superName, [fields with defaults]]
  var EVENT_CLASSES = {
    UIEvent: ['Event', { detail: 0, view: null, which: 0 }],
    CustomEvent: ['Event', { detail: null }],
    MouseEvent: ['UIEvent', { screenX: 0, screenY: 0, clientX: 0, clientY: 0, pageX: 0, pageY: 0, offsetX: 0, offsetY: 0, x: 0, y: 0, movementX: 0, movementY: 0, button: 0, buttons: 0, ctrlKey: false, shiftKey: false, altKey: false, metaKey: false, relatedTarget: null }],
    PointerEvent: ['MouseEvent', { pointerId: 1, width: 1, height: 1, pressure: 0, tiltX: 0, tiltY: 0, pointerType: 'mouse', isPrimary: true }],
    WheelEvent: ['MouseEvent', { deltaX: 0, deltaY: 0, deltaZ: 0, deltaMode: 0 }],
    DragEvent: ['MouseEvent', { dataTransfer: null }],
    KeyboardEvent: ['UIEvent', { key: '', code: '', location: 0, repeat: false, isComposing: false, charCode: 0, keyCode: 0, ctrlKey: false, shiftKey: false, altKey: false, metaKey: false }],
    InputEvent: ['UIEvent', { data: null, inputType: '', isComposing: false, dataTransfer: null }],
    CompositionEvent: ['UIEvent', { data: '' }],
    FocusEvent: ['UIEvent', { relatedTarget: null }],
    TouchEvent: ['UIEvent', { touches: [], targetTouches: [], changedTouches: [], ctrlKey: false, shiftKey: false, altKey: false, metaKey: false }],
    ProgressEvent: ['Event', { lengthComputable: false, loaded: 0, total: 0 }],
    ErrorEvent: ['Event', { message: '', filename: '', lineno: 0, colno: 0, error: null }],
    PromiseRejectionEvent: ['Event', { promise: null, reason: undefined }],
    MessageEvent: ['Event', { data: null, origin: '', lastEventId: '', source: null, ports: [] }],
    PopStateEvent: ['Event', { state: null }],
    HashChangeEvent: ['Event', { oldURL: '', newURL: '' }],
    PageTransitionEvent: ['Event', { persisted: false }],
    AnimationEvent: ['Event', { animationName: '', elapsedTime: 0, pseudoElement: '' }],
    TransitionEvent: ['Event', { propertyName: '', elapsedTime: 0, pseudoElement: '' }],
    StorageEvent: ['Event', { key: null, oldValue: null, newValue: null, url: '', storageArea: null }],
    BeforeUnloadEvent: ['Event', {}],
    CloseEvent: ['Event', { wasClean: false, code: 0, reason: '' }],
    SubmitEvent: ['Event', { submitter: null }],
    ClipboardEvent: ['Event', { clipboardData: null }],
    ToggleEvent: ['Event', { oldState: '', newState: '' }],
    BeforeToggleEvent: ['ToggleEvent', {}]
  };
  var eventFieldsOf = { Event: {} };
  (function buildEventClasses() {
    var pending = Object.keys(EVENT_CLASSES);
    while (pending.length) {
      var name = pending.shift();
      var spec = EVENT_CLASSES[name];
      if (!global[spec[0]]) { pending.push(name); continue; }
      var sup = global[spec[0]];
      var fields = Object.assign({}, eventFieldsOf[spec[0]], spec[1]);
      eventFieldsOf[name] = fields;
      var C = (function (name, sup, fields) {
        var C = function (type, init) {
          if (!(this instanceof C)) throw new TypeError("Constructor " + name + " requires 'new'");
          sup.call(this, type, init);
          init = init || {};
          for (var f in fields) this[f] = f in init ? init[f] : (Array.isArray(fields[f]) ? [] : fields[f]);
          if (name === 'MouseEvent' || fields.clientX !== undefined) { if (!('x' in init)) this.x = this.clientX; if (!('y' in init)) this.y = this.clientY; if (!('pageX' in init)) this.pageX = this.clientX; if (!('pageY' in init)) this.pageY = this.clientY; }
          if (fields.keyCode !== undefined && !('keyCode' in init) && this.key) { this.keyCode = this.which = keyCodeFor(this.key); }
        };
        return C;
      })(name, sup, fields);
      extend(C, sup);
      setTag(C, name);
      def(C, 'name', { value: name, writable: false, enumerable: false });
      global[name] = C;
    }
  })();
  methods(global.UIEvent.prototype, { initUIEvent: function (t, b, c, view, detail) { this.initEvent(t, b, c); this.view = view || null; this.detail = detail || 0; } });
  methods(global.CustomEvent.prototype, { initCustomEvent: function (t, b, c, detail) { this.initEvent(t, b, c); this.detail = detail === undefined ? null : detail; } });
  methods(global.MouseEvent.prototype, { initMouseEvent: function (t, b, c, view, detail, sx, sy, cx, cy, ctrl, alt, shift, meta, button, related) { this.initEvent(t, b, c); this.detail = detail || 0; this.clientX = this.x = this.pageX = cx || 0; this.clientY = this.y = this.pageY = cy || 0; this.screenX = sx || 0; this.screenY = sy || 0; this.ctrlKey = !!ctrl; this.altKey = !!alt; this.shiftKey = !!shift; this.metaKey = !!meta; this.button = button || 0; this.relatedTarget = related || null; }, getModifierState: function (k) { return k === 'Shift' ? this.shiftKey : k === 'Control' ? this.ctrlKey : k === 'Alt' ? this.altKey : k === 'Meta' ? this.metaKey : false; } });
  methods(global.KeyboardEvent.prototype, { getModifierState: global.MouseEvent.prototype.getModifierState, initKeyboardEvent: function (t, b, c, view, key) { this.initEvent(t, b, c); this.key = key || ''; } });
  constant(global.KeyboardEvent, 'DOM_KEY_LOCATION_STANDARD', 0); constant(global.KeyboardEvent, 'DOM_KEY_LOCATION_LEFT', 1); constant(global.KeyboardEvent, 'DOM_KEY_LOCATION_RIGHT', 2); constant(global.KeyboardEvent, 'DOM_KEY_LOCATION_NUMPAD', 3);
  constant(global.WheelEvent, 'DOM_DELTA_PIXEL', 0); constant(global.WheelEvent, 'DOM_DELTA_LINE', 1); constant(global.WheelEvent, 'DOM_DELTA_PAGE', 2);

  var KEY_CODES = { Enter: 13, Tab: 9, Escape: 27, Backspace: 8, Delete: 46, ArrowLeft: 37, ArrowUp: 38, ArrowRight: 39, ArrowDown: 40, Home: 36, End: 35, PageUp: 33, PageDown: 34, Shift: 16, Control: 17, Alt: 18, Meta: 91, CapsLock: 20, Insert: 45, ' ': 32 };
  function keyCodeFor(key) {
    if (KEY_CODES[key] !== undefined) return KEY_CODES[key];
    if (key.length === 1) { var c = key.toUpperCase().charCodeAt(0); return c; }
    if (/^F\d+$/.test(key)) return 111 + parseInt(key.slice(1), 10);
    return 0;
  }
  function codeFor(key) {
    if (key.length === 1) {
      if (/[a-zA-Z]/.test(key)) return 'Key' + key.toUpperCase();
      if (/[0-9]/.test(key)) return 'Digit' + key;
      if (key === ' ') return 'Space';
      var punct = { '.': 'Period', ',': 'Comma', '-': 'Minus', '=': 'Equal', ';': 'Semicolon', "'": 'Quote', '/': 'Slash', '\\': 'Backslash', '[': 'BracketLeft', ']': 'BracketRight', '`': 'Backquote' };
      return punct[key] || 'Unidentified';
    }
    if (key === 'Shift' || key === 'Control' || key === 'Alt' || key === 'Meta') return key + 'Left';
    return key;
  }
  // choose the event class from the type name (used by __swDispatch)
  function eventClassForType(type) {
    if (/^(click|dblclick|mouse|contextmenu|auxclick)/.test(type)) return global.MouseEvent;
    if (/^pointer|^gotpointer|^lostpointer/.test(type)) return global.PointerEvent;
    if (/^key/.test(type)) return global.KeyboardEvent;
    if (type === 'input' || type === 'beforeinput') return global.InputEvent;
    if (/^(focus|blur|focusin|focusout)$/.test(type)) return global.FocusEvent;
    if (/^touch/.test(type)) return global.TouchEvent;
    if (/^drag|^drop$/.test(type)) return global.DragEvent;
    if (type === 'wheel' || type === 'mousewheel') return global.WheelEvent;
    if (/^composition/.test(type)) return global.CompositionEvent;
    if (type === 'submit') return global.SubmitEvent;
    if (type === 'error') return global.ErrorEvent;
    if (type === 'popstate') return global.PopStateEvent;
    if (type === 'hashchange') return global.HashChangeEvent;
    if (type === 'message') return global.MessageEvent;
    if (type === 'pageshow' || type === 'pagehide') return global.PageTransitionEvent;
    if (type === 'storage') return global.StorageEvent;
    if (/^(load|progress|abort|loadstart|loadend|timeout)$/.test(type)) return global.ProgressEvent;
    return global.Event;
  }
  var BUBBLING_DEFAULT = /^(click|dblclick|mousedown|mouseup|mousemove|mouseover|mouseout|contextmenu|auxclick|wheel|pointer(down|up|move|over|out|cancel)|key(down|up|press)|input|beforeinput|change|submit|reset|focusin|focusout|touch(start|end|move|cancel)|drag.*|drop|select|copy|cut|paste|compositionstart|compositionupdate|compositionend|animation.*|transition.*|toggle|DOMContentLoaded|readystatechange|hashchange|popstate|message|error|invalid|scroll|resize)$/;
  var CANCELABLE_DEFAULT = /^(click|dblclick|mousedown|mouseup|contextmenu|auxclick|wheel|pointerdown|pointerup|keydown|keyup|keypress|beforeinput|submit|reset|touchstart|touchend|touchmove|drag.*|drop|beforeunload|copy|cut|paste|select|invalid)$/;
  function makeEventFor(type, init) {
    init = Object.assign({}, init || {});
    if (!('bubbles' in init)) init.bubbles = BUBBLING_DEFAULT.test(type);
    if (!('cancelable' in init)) init.cancelable = CANCELABLE_DEFAULT.test(type);
    var C = eventClassForType(type);
    if (C === global.KeyboardEvent && init.key && !init.code) init.code = codeFor(init.key);
    if (init.relatedTarget && typeof init.relatedTarget === 'number') init.relatedTarget = wrap(init.relatedTarget);
    var ev = new C(type, init);
    ev.isTrusted = true;
    return ev;
  }

  // ---- EventTarget --------------------------------------------------------
  function EventTarget() { }
  function listenerMap(t) {
    var m = t[LISTENERS];
    if (!m) { m = Object.create(null); Object.defineProperty(t, LISTENERS, { value: m, configurable: true, writable: true }); }
    return m;
  }
  function normOptions(options) {
    if (typeof options === 'boolean') return { capture: options, once: false, passive: false, passiveUnset: true, signal: null };
    options = options || {};
    return { capture: !!options.capture, once: !!options.once, passive: !!options.passive, passiveUnset: options.passive === undefined, signal: options.signal || null };
  }
  EventTarget.prototype = {
    constructor: EventTarget,
    addEventListener: function (type, listener, options) {
      var o = normOptions(options); // read options first: feature probes use getters on `passive`
      if (!listener || (typeof listener !== 'function' && typeof listener !== 'object')) return;
      var list = listenerMap(this)[type] || (listenerMap(this)[type] = []);
      for (var i = 0; i < list.length; i++) if (list[i].fn === listener && list[i].capture === o.capture) return;
      if (o.passiveUnset && /^(wheel|mousewheel|touchstart|touchmove)$/.test(type) && (this === global || this === documentWrapper || this === documentWrapper.documentElement || this === documentWrapper.body)) o.passive = true; // the default of window, document and body listeners
      var rec = { fn: listener, capture: o.capture, once: o.once, passive: o.passive, removed: false };
      if (!o.passive && /^(wheel|mousewheel)$/.test(type)) wheelNP = true;
      list.push(rec);
      if (o.signal) {
        if (o.signal.aborted) { rec.removed = true; list.splice(list.indexOf(rec), 1); return; }
        var self = this;
        o.signal.addEventListener('abort', function () { self.removeEventListener(type, listener, o.capture); });
      }
    },
    removeEventListener: function (type, listener, options) {
      var m = this[LISTENERS]; if (!m) return;
      var list = m[type]; if (!list) return;
      var capture = typeof options === 'boolean' ? options : !!(options && options.capture);
      for (var i = 0; i < list.length; i++) if (list[i].fn === listener && list[i].capture === capture) { list[i].removed = true; list.splice(i, 1); return; }
    },
    dispatchEvent: function (ev) {
      if (!(ev instanceof Event)) throw new TypeError("Failed to execute 'dispatchEvent': parameter 1 is not of type 'Event'.");
      if (ev._dispatching) throw DOMException('The event is already being dispatched.', 'InvalidStateError');
      return dispatch(this, ev);
    }
  };
  setTag(EventTarget, 'EventTarget');
  global.EventTarget = EventTarget;

  function eventPath(target) {
    var path = [target];
    if (isNodeObj(target)) {
      var id = target[ID];
      if (target.nodeType === 11) { /* fragments do not bubble */ }
      else {
        var p = sw.parent(id);
        while (p) { var w = wrap(p); if (!w) break; path.push(w); if (w.nodeType === 9) break; p = sw.parent(p); }
        var last = path[path.length - 1];
        if (last !== documentWrapper && documentWrapper && sw.attached(id)) path.push(documentWrapper);
        if (last === documentWrapper || sw.attached(id) || target === documentWrapper) path.push(global);
      }
    }
    return path;
  }
  // Inline event handler attributes (onclick="..."): compiled on first use and again when the attribute changes.
  // The scope is the element, then the document, as in browsers (the form owner is left out).
  var attrHandlers = new WeakMap();
  var scriptsBlocked = false; // a frame sandboxed without allow-scripts: its markup's inline handlers must not run
  function attributeHandler(el, type) {
    if (scriptsBlocked) return undefined;
    var src = el.getAttribute('on' + type);
    if (src === null || src === undefined) return undefined;
    var per = attrHandlers.get(el);
    if (!per) { per = Object.create(null); attrHandlers.set(el, per); }
    var c = per[type];
    if (c && c.src === src) return c.fn;
    var fn = null;
    try { fn = new Function('event', 'with (document) { with (this) { ' + src + '\n } }'); } catch (e) { reportError(e); }
    per[type] = { src: src, fn: fn };
    return fn;
  }
  function invokeListeners(target, ev, phase) {
    var m = target[LISTENERS];
    var list = m && m[ev.type];
    ev.currentTarget = target;
    ev.eventPhase = phase;
    if (list && list.length) {
      var snapshot = list.slice();
      for (var i = 0; i < snapshot.length; i++) {
        var rec = snapshot[i];
        if (rec.removed) continue;
        if (phase === 1 && !rec.capture) continue;
        if (phase === 3 && rec.capture) continue;
        if (rec.once) target.removeEventListener(ev.type, rec.fn, rec.capture);
        ev._passive = rec.passive;
        try {
          if (typeof rec.fn === 'function') rec.fn.call(target, ev);
          else if (typeof rec.fn.handleEvent === 'function') rec.fn.handleEvent(ev);
        } catch (e) { reportError(e); }
        ev._passive = false;
        if (ev._stopNow) break;
      }
    }
    // legacy on* handler runs with the bubble/target listeners
    if (phase !== 1 && !ev._stopNow) {
      var h = target[HANDLERS] && target[HANDLERS][ev.type];
      if (h === undefined && target !== global && typeof target.getAttribute === 'function') h = attributeHandler(target, ev.type);
      if (typeof h === 'function') {
        try {
          var r = target === global && ev.type === 'error' && ev instanceof global.ErrorEvent ? h.call(target, ev.message, ev.filename, ev.lineno, ev.colno, ev.error) : h.call(target, ev);
          if (r === false && ev.type !== 'error') ev.preventDefault();
          if (ev.type === 'error' && r === true) ev.preventDefault();
          if (ev.type === 'beforeunload' && typeof r === 'string') ev.returnValue = r;
        } catch (e) { reportError(e); }
      }
    }
  }
  function dispatch(target, ev) {
    ev._dispatching = true;
    ev.target = ev.srcElement = target;
    ev._stop = false; ev._stopNow = false;
    var path = eventPath(target);
    ev._path = path;
    var i;
    for (i = path.length - 1; i > 0 && !ev._stop; i--) invokeListeners(path[i], ev, 1);
    if (!ev._stop) invokeListeners(target, ev, 2);
    if (ev.bubbles) for (i = 1; i < path.length && !ev._stop; i++) invokeListeners(path[i], ev, 3);
    ev.eventPhase = 0; ev.currentTarget = null; ev._dispatching = false;
    return !ev.defaultPrevented;
  }
  function fire(target, type, init) { return dispatch(target, makeEventFor(type, init)); }

  // Errors from listeners/timers/microtasks: log + window 'error' event, never throw.
  // Text for an uncaught value. The engine's stacks carry no "Name: message" header, so an Error is printed as
  // "Name: message" and then its stack (a stack that already shows the message is kept as is); anything else that can
  // be thrown (a string, a number, null, a plain object) has no stack, so say what it is and what its type is.
  function describeThrown(e) {
    try {
      if (e instanceof Error || (e !== null && typeof e === 'object' && typeof e.name === 'string' && typeof e.message === 'string')) {
        var head = String(e.name || 'Error') + ': ' + String(e.message), stack = typeof e.stack === 'string' ? e.stack : '';
        if (!stack) return head;
        return e.message !== '' && stack.indexOf(String(e.message)) >= 0 ? stack : head + '\n' + stack;
      }
      if (e !== null && (typeof e === 'object' || typeof e === 'function')) {
        var shown;
        try { shown = JSON.stringify(e); } catch (x) { /* circular */ }
        if (shown === undefined) { try { shown = String(e); } catch (x) { shown = Object.prototype.toString.call(e); } }
        return 'non-Error ' + typeof e + ' thrown (' + ((e.constructor && e.constructor.name) || 'Object') + '): ' + shown + (typeof e.stack === 'string' ? '\n' + e.stack : '');
      }
      return 'non-Error ' + typeof e + ' thrown: ' + (typeof e === 'string' ? JSON.stringify(e) : String(e));
    } catch (x) { return 'unprintable value thrown'; }
  }
  var reportingError = false;
  function reportError(e) {
    hostLog('error', 'Uncaught ' + describeThrown(e), '');
    if (reportingError) return;
    reportingError = true;
    try {
      var ev = new global.ErrorEvent('error', { message: e && e.message !== undefined ? String(e.message) : String(e), error: e, filename: (e && e.fileName) || '', lineno: (e && e.lineNumber) || 0, cancelable: true });
      dispatch(global, ev);
      if (typeof global.__swUncaught === 'function' && !ev.defaultPrevented) global.__swUncaught(ev); // a worker forwards what it did not handle to its owner
    } catch (e2) { /* ignore */ } finally { reportingError = false; }
  }

  // on* handler properties
  var HANDLERS = Symbol('handlers');
  var COMMON_EVENTS = ('abort animationend animationiteration animationstart auxclick beforeinput blur cancel canplay canplaythrough change click close contextmenu copy cuechange cut dblclick drag dragend dragenter dragleave dragover dragstart drop durationchange emptied ended error focus focusin focusout formdata gotpointercapture input invalid keydown keypress keyup load loadeddata loadedmetadata loadstart lostpointercapture mousedown mouseenter mouseleave mousemove mouseout mouseover mouseup paste pause play playing pointercancel pointerdown pointerenter pointerleave pointermove pointerout pointerover pointerup progress ratechange reset resize scroll scrollend securitypolicyviolation seeked seeking select selectionchange selectstart slotchange stalled submit suspend timeupdate toggle touchcancel touchend touchmove touchstart transitioncancel transitionend transitionrun transitionstart volumechange waiting wheel').split(' ');
  var WINDOW_EVENTS = ('afterprint beforeprint beforeunload hashchange languagechange message messageerror offline online pagehide pageshow popstate rejectionhandled storage unhandledrejection unload DOMContentLoaded readystatechange visibilitychange fullscreenchange').split(' ');
  function defineHandlerProps(proto, names) {
    names.forEach(function (name) {
      getter(proto, 'on' + name.toLowerCase(), function () { var h = this[HANDLERS]; return h && h[name] !== undefined ? h[name] : null; }, function (v) {
        var h = this[HANDLERS];
        if (!h) { h = Object.create(null); Object.defineProperty(this, HANDLERS, { value: h, configurable: true }); }
        h[name] = typeof v === 'function' || (v && typeof v === 'object') ? v : null;
      });
    });
  }

  // ==== 4. Collections =====================================================
  // Array-like collections. NodeList/HTMLCollection are snapshots taken at
  // creation but every DOM accessor re-reads the host, so `el.childNodes` is
  // always fresh on the next access ("live-ish"). A `live` variant backed by
  // a producer function is used for childNodes/children so `length` re-reads.
  // hideMethods makes prototype members non-enumerable so for-in over a
  // collection (jQuery.map's fallback path) sees only the indexed items.
  function hideMethods(proto) {
    Object.getOwnPropertyNames(proto).forEach(function (k) {
      var d = Object.getOwnPropertyDescriptor(proto, k);
      if (d && d.enumerable && d.configurable) { d.enumerable = false; Object.defineProperty(proto, k, d); }
    });
  }
  function arrayLike(C, items) {
    var o = Object.create(C.prototype);
    for (var i = 0; i < items.length; i++) o[i] = items[i];
    Object.defineProperty(o, 'length', { value: items.length, configurable: true });
    return o;
  }
  // genCached: a producer's result is reused until the document changes. A live collection answers
  // every list[i] and list.length by calling its producer, so a loop over getElementsByTagName('*')
  // (jQuery/Sizzle do exactly that) re-read the whole tree once per index: quadratic, seconds on a
  // long page. The document's mutation counter says when the answer can differ.
  // treeCached is genCached keyed on structural mutations only (sw.treeGen): for lists whose membership depends
  // only on tree shape and immutable node properties, so attribute/style writes do not force a rebuild.
  function treeCached(fn) {
    if (!host('treeGen')) return genCached(fn);
    var g = -1, v = null;
    return function () {
      var cur = sw.treeGen();
      if (v === null || cur !== g) { v = fn(); g = cur; }
      return v;
    };
  }
  function genCached(fn) {
    var g = -1, v = null;
    return function () {
      var cur = host('docGen') ? sw.docGen() : -1;
      if (cur < 0) return fn();
      if (v === null || cur !== g) { v = fn(); g = cur; }
      return v;
    };
  }
  function liveList(C, producer) {
    var target = Object.create(C.prototype);
    return new Proxy(target, {
      get: function (t, k) {
        if (k === 'length') return producer().length;
        if (typeof k === 'string' && /^\d+$/.test(k)) return producer()[+k];
        if (k === '_items') return producer();
        if (C === HTMLCollection && typeof k === 'string' && !(k in t)) { var items = producer(); for (var i = 0; i < items.length; i++) if (items[i].id === k || items[i].getAttribute('name') === k) return items[i]; }
        return t[k];
      },
      has: function (t, k) { if (k === 'length' || k === '_items') return true; if (typeof k === 'string' && /^\d+$/.test(k)) return +k < producer().length; return k in t; },
      ownKeys: function (t) { var keys = []; var n = producer().length; for (var i = 0; i < n; i++) keys.push(String(i)); return keys.concat(['length']); },
      getOwnPropertyDescriptor: function (t, k) {
        if (k === 'length') return { value: producer().length, configurable: true, enumerable: false, writable: false };
        if (typeof k === 'string' && /^\d+$/.test(k)) { var v = producer()[+k]; return v === undefined ? undefined : { value: v, configurable: true, enumerable: true, writable: false }; }
        return Object.getOwnPropertyDescriptor(t, k);
      }
    });
  }
  function itemsOf(list) { return list._items || slice.call(list); }

  function NodeList() { illegal(); }
  NodeList.prototype = {
    constructor: NodeList,
    item: function (i) { i = +i; return i >= 0 && i < this.length ? this[i] : null; },
    forEach: function (fn, thisArg) { var items = itemsOf(this); for (var i = 0; i < items.length; i++) fn.call(thisArg, items[i], i, this); },
    entries: function () { return itemsOf(this).entries(); },
    keys: function () { return itemsOf(this).keys(); },
    values: function () { return itemsOf(this).values(); }
  };
  NodeList.prototype[Symbol.iterator] = NodeList.prototype.values;
  hideMethods(NodeList.prototype);
  setTag(NodeList, 'NodeList');
  global.NodeList = NodeList;

  function HTMLCollection() { illegal(); }
  HTMLCollection.prototype = {
    constructor: HTMLCollection,
    item: NodeList.prototype.item,
    namedItem: function (name) { var items = itemsOf(this); for (var i = 0; i < items.length; i++) if (items[i].id === name || items[i].getAttribute('name') === name) return items[i]; return null; }
  };
  HTMLCollection.prototype[Symbol.iterator] = function () { return itemsOf(this).values(); };
  hideMethods(HTMLCollection.prototype);
  setTag(HTMLCollection, 'HTMLCollection');
  global.HTMLCollection = HTMLCollection;

  function nodeListOf(items) { return arrayLike(NodeList, items); }
  function collectionOf(items) { return arrayLike(HTMLCollection, items); }

  // ---- DOMTokenList (classList, relList, ...) -------------------------------
  function DOMTokenList() { illegal(); }
  function tokenList(el, attr) {
    var list = Object.create(DOMTokenList.prototype);
    Object.defineProperty(list, '_el', { value: el, configurable: true });
    Object.defineProperty(list, '_attr', { value: attr, configurable: true });
    return new Proxy(list, {
      get: function (t, k) {
        if (k === 'length') return t._tokens().length;
        if (typeof k === 'string' && /^\d+$/.test(k)) return t._tokens()[+k];
        return t[k];
      },
      ownKeys: function (t) { var n = t._tokens().length, keys = []; for (var i = 0; i < n; i++) keys.push(String(i)); return keys.concat(['length']); },
      getOwnPropertyDescriptor: function (t, k) {
        if (typeof k === 'string' && /^\d+$/.test(k)) { var v = t._tokens()[+k]; return v === undefined ? undefined : { value: v, configurable: true, enumerable: true, writable: false }; }
        if (k === 'length') return { value: t._tokens().length, configurable: true, enumerable: false };
        return Object.getOwnPropertyDescriptor(t, k);
      }
    });
  }
  function splitTokens(s) { return s ? String(s).split(/\s+/).filter(function (t) { return t; }).filter(function (t, i, a) { return a.indexOf(t) === i; }) : []; }
  function checkToken(tok, name) {
    tok = String(tok);
    if (tok === '') throw DOMException("Failed to execute '" + name + "' on 'DOMTokenList': The token provided must not be empty.", 'SyntaxError');
    if (/\s/.test(tok)) throw DOMException("Failed to execute '" + name + "' on 'DOMTokenList': The token provided ('" + tok + "') contains HTML space characters, which are not valid in tokens.", 'InvalidCharacterError');
    return tok;
  }
  DOMTokenList.prototype = {
    constructor: DOMTokenList,
    _tokens: function () { return splitTokens(sw.getAttr(this._el[ID], this._attr) || ''); },
    _write: function (tokens) { this._el.setAttribute(this._attr, tokens.join(' ')); },
    get value() { return sw.getAttr(this._el[ID], this._attr) || ''; },
    set value(v) { this._el.setAttribute(this._attr, String(v)); },
    item: function (i) { var t = this._tokens(); return i >= 0 && i < t.length ? t[i] : null; },
    contains: function (tok) { return this._tokens().indexOf(String(tok)) >= 0; },
    add: function () { var t = this._tokens(), changed = false; for (var i = 0; i < arguments.length; i++) { var tok = checkToken(arguments[i], 'add'); if (t.indexOf(tok) < 0) { t.push(tok); changed = true; } } if (changed) this._write(t); },
    remove: function () { var t = this._tokens(), changed = false; for (var i = 0; i < arguments.length; i++) { var tok = checkToken(arguments[i], 'remove'); var at = t.indexOf(tok); if (at >= 0) { t.splice(at, 1); changed = true; } } if (changed) this._write(t); },
    toggle: function (tok, force) {
      tok = checkToken(tok, 'toggle');
      var has = this.contains(tok);
      if (force === undefined) force = !has;
      if (force && !has) this.add(tok); else if (!force && has) this.remove(tok);
      return !!force;
    },
    replace: function (a, b) { a = checkToken(a, 'replace'); b = checkToken(b, 'replace'); var t = this._tokens(); var at = t.indexOf(a); if (at < 0) return false; t[at] = b; this._write(t.filter(function (x, i, arr) { return arr.indexOf(x) === i; })); return true; },
    supports: function () { return true; },
    forEach: function (fn, thisArg) { this._tokens().forEach(function (t, i) { fn.call(thisArg, t, i, this); }, this); },
    entries: function () { return this._tokens().entries(); },
    keys: function () { return this._tokens().keys(); },
    values: function () { return this._tokens().values(); },
    toString: function () { return this.value; }
  };
  DOMTokenList.prototype[Symbol.iterator] = DOMTokenList.prototype.values;
  setTag(DOMTokenList, 'DOMTokenList');
  global.DOMTokenList = DOMTokenList;

  // ---- Attr / NamedNodeMap (minimal) ------------------------------------
  function Attr() { illegal(); }
  Attr.prototype = {
    constructor: Attr, nodeType: 2,
    get value() { return sw.getAttr(this.ownerElement[ID], this.name) || ''; },
    set value(v) { this.ownerElement.setAttribute(this.name, v); },
    get nodeValue() { return this.value; }, set nodeValue(v) { this.value = v; },
    get textContent() { return this.value; },
    get nodeName() { return this.name; }, get localName() { return this.name; },
    get specified() { return true; }, get namespaceURI() { return null; }, get prefix() { return null; },
    toString: function () { return '[object Attr]'; }
  };
  setTag(Attr, 'Attr'); global.Attr = Attr;
  function attrOf(el, name) {
    var a = Object.create(Attr.prototype);
    Object.defineProperty(a, 'name', { value: name, enumerable: true });
    Object.defineProperty(a, 'ownerElement', { value: el, enumerable: true });
    return a;
  }
  function NamedNodeMap() { illegal(); }
  NamedNodeMap.prototype = {
    constructor: NamedNodeMap,
    _names: function () { return sw.attrNames(this._el[ID]) || []; },
    item: function (i) { var n = this._names(); return i >= 0 && i < n.length ? attrOf(this._el, n[i]) : null; },
    getNamedItem: function (name) { return sw.hasAttr(this._el[ID], String(name).toLowerCase()) ? attrOf(this._el, String(name).toLowerCase()) : null; },
    setNamedItem: function (attr) { this._el.setAttribute(attr.name, attr.value); return null; },
    removeNamedItem: function (name) { var a = this.getNamedItem(name); if (!a) throw DOMException('No item with name ' + name, 'NotFoundError'); this._el.removeAttribute(name); return a; }
  };
  NamedNodeMap.prototype[Symbol.iterator] = function () { var el = this._el; return this._names().map(function (n) { return attrOf(el, n); }).values(); };
  setTag(NamedNodeMap, 'NamedNodeMap'); global.NamedNodeMap = NamedNodeMap;
  function namedNodeMap(el) {
    var m = Object.create(NamedNodeMap.prototype);
    Object.defineProperty(m, '_el', { value: el, configurable: true });
    // The index properties have to be visible to `in`, Object.keys and getOwnPropertyDescriptor, not only to `get`:
    // Array.prototype.map/forEach/slice.call(el.attributes) skip an index that `in` denies, and returned empty arrays.
    return new Proxy(m, {
      get: function (t, k) {
        if (k === 'length') return t._names().length;
        if (typeof k === 'string' && /^\d+$/.test(k)) return t.item(+k);
        if (typeof k === 'string' && !(k in t) && sw.hasAttr(el[ID], k)) return attrOf(el, k);
        return t[k];
      },
      has: function (t, k) {
        if (k === 'length') return true;
        if (typeof k === 'string' && /^\d+$/.test(k)) return +k < t._names().length;
        return k in t;
      },
      ownKeys: function (t) { var keys = [], n = t._names().length; for (var i = 0; i < n; i++) keys.push(String(i)); return keys; },
      getOwnPropertyDescriptor: function (t, k) {
        if (typeof k === 'string' && /^\d+$/.test(k)) { var a = t.item(+k); return a ? { value: a, configurable: true, enumerable: true, writable: false } : undefined; }
        return Object.getOwnPropertyDescriptor(t, k);
      }
    });
  }

  // ---- CSSStyleDeclaration backed by the style attribute -------------------
  var CSS_PROPS = ('align-content align-items align-self animation appearance aspect-ratio backdrop-filter background background-attachment background-clip background-color background-image background-origin background-position background-repeat background-size border border-bottom border-bottom-color border-bottom-left-radius border-bottom-right-radius border-bottom-style border-bottom-width border-collapse border-color border-left border-left-color border-left-style border-left-width border-radius border-right border-right-color border-right-style border-right-width border-spacing border-style border-top border-top-color border-top-left-radius border-top-right-radius border-top-style border-top-width border-width bottom box-shadow box-sizing caret-color clear clip clip-path color column-gap columns content cursor direction display fill filter flex flex-basis flex-direction flex-flow flex-grow flex-shrink flex-wrap float font font-family font-size font-style font-variant font-weight gap grid grid-area grid-auto-columns grid-auto-flow grid-auto-rows grid-column grid-column-end grid-column-start grid-gap grid-row grid-row-end grid-row-start grid-template grid-template-areas grid-template-columns grid-template-rows height inset isolation justify-content justify-items justify-self left letter-spacing line-height list-style list-style-image list-style-position list-style-type margin margin-bottom margin-left margin-right margin-top mask max-height max-width min-height min-width mix-blend-mode object-fit object-position opacity order outline outline-color outline-offset outline-style outline-width overflow overflow-wrap overflow-x overflow-y padding padding-bottom padding-left padding-right padding-top perspective pointer-events position resize right row-gap scroll-behavior stroke stroke-width tab-size table-layout text-align text-decoration text-decoration-color text-decoration-line text-decoration-style text-indent text-overflow text-shadow text-transform top touch-action transform transform-origin transition transition-delay transition-duration transition-property transition-timing-function user-select vertical-align visibility white-space width will-change word-break word-spacing word-wrap writing-mode z-index').split(' ');
  function camel(s) { return s.replace(/^-ms-/, 'ms-').replace(/-([a-z])/g, function (_, c) { return c.toUpperCase(); }); }
  function kebab(s) { return s.replace(/^(webkit|moz|ms)([A-Z])/, '-$1$2').replace(/[A-Z]/g, function (c) { return '-' + c.toLowerCase(); }); }
  function parseCss(text) {
    var out = []; // [{name, value, important}]
    if (!text) return out;
    text.split(';').forEach(function (decl) {
      var i = decl.indexOf(':'); if (i < 0) return;
      var name = decl.slice(0, i).trim().toLowerCase(), value = decl.slice(i + 1).trim(), imp = false;
      if (!name) return;
      var m = /^(.*?)\s*!\s*important$/i.exec(value); if (m) { value = m[1]; imp = true; }
      value = canonNumbers(value);
      for (var j = 0; j < out.length; j++) if (out[j].name === name) { out.splice(j, 1); break; }
      out.push({ name: name, value: value, important: imp });
    });
    return out;
  }
  // numbers in a declared value serialize the way browsers do (".5" -> "0.5", "1.50px" -> "1.5px", "+2" -> "2"):
  // jQuery's support tests read style.opacity back after writing "opacity:.5" and switch to IE filters otherwise
  var NUM_RE = /(^|[\s,(\/])([+-]?)(\d*)\.?(\d*)(?=[a-z%]*(?=$|[\s,)\/]))/gi;
  function canonNumbers(v) {
    if (v.indexOf('.') < 0 && v.indexOf('+') < 0 || /["'\\]|url\(/i.test(v)) return v;
    return v.replace(NUM_RE, function (all, pre, sign, int, frac, off, str) {
      if (int === '' && frac === '') return all;
      if (str.charAt(off + all.length) === '.') return all; // "1.2.3": not a number
      var dot = all.indexOf('.') >= 0;
      if (!dot && sign !== '+') return all;
      frac = dot ? frac.replace(/0+$/, '') : '';
      int = int.replace(/^0+(?=\d)/, '') || '0';
      return pre + (sign === '-' ? '-' : '') + int + (frac ? '.' + frac : '');
    });
  }
  function serializeCss(decls) { return decls.map(function (d) { return d.name + ': ' + d.value + (d.important ? ' !important' : '') + ';'; }).join(' '); }

  function CSSStyleDeclaration() { illegal(); }
  CSSStyleDeclaration.prototype = {
    constructor: CSSStyleDeclaration,
    _read: function () { return parseCss(this._el ? sw.getAttr(this._el[ID], 'style') : this._text); },
    _write: function (decls) {
      var text = serializeCss(decls);
      if (this._el) { if (text) this._el.setAttribute('style', text); else if (this._el.hasAttribute('style')) this._el.removeAttribute('style'); }
      else this._text = text;
    },
    get cssText() { return serializeCss(this._read()); },
    set cssText(v) { this._write(parseCss(String(v))); },
    get length() { return this._read().length; },
    get parentRule() { return null; },
    item: function (i) { var d = this._read(); return i >= 0 && i < d.length ? d[i].name : ''; },
    getPropertyValue: function (name) { name = String(name).toLowerCase(); var d = this._read(); for (var i = 0; i < d.length; i++) if (d[i].name === name) return d[i].value; return ''; },
    getPropertyPriority: function (name) { name = String(name).toLowerCase(); var d = this._read(); for (var i = 0; i < d.length; i++) if (d[i].name === name) return d[i].important ? 'important' : ''; return ''; },
    setProperty: function (name, value, priority) {
      name = String(name).toLowerCase();
      if (value === null || value === undefined || value === '') { this.removeProperty(name); return; }
      var d = this._read(), rec = { name: name, value: String(value).trim(), important: priority === 'important' }, found = false;
      for (var i = 0; i < d.length; i++) if (d[i].name === name) { d[i] = rec; found = true; break; }
      if (!found) d.push(rec);
      this._write(d);
    },
    removeProperty: function (name) {
      name = String(name).toLowerCase();
      var d = this._read(), old = '';
      for (var i = 0; i < d.length; i++) if (d[i].name === name) { old = d[i].value; d.splice(i, 1); this._write(d); break; }
      return old;
    },
    toString: function () { return '[object CSSStyleDeclaration]'; }
  };
  CSS_PROPS.forEach(function (prop) {
    var cc = camel(prop);
    var desc = { get: function () { return this.getPropertyValue(prop); }, set: function (v) { this.setProperty(prop, v); }, enumerable: true, configurable: true };
    Object.defineProperty(CSSStyleDeclaration.prototype, cc, desc);
    if (cc !== prop) Object.defineProperty(CSSStyleDeclaration.prototype, prop, desc);
    ['webkit', 'moz', 'ms'].forEach(function (vendor) { Object.defineProperty(CSSStyleDeclaration.prototype, vendor + cc[0].toUpperCase() + cc.slice(1), { get: function () { return this.getPropertyValue('-' + vendor + '-' + prop); }, set: function (v) { this.setProperty('-' + vendor + '-' + prop, v); }, configurable: true }); });
  });
  Object.defineProperty(CSSStyleDeclaration.prototype, 'cssFloat', { get: function () { return this.getPropertyValue('float'); }, set: function (v) { this.setProperty('float', v); }, configurable: true });
  CSSStyleDeclaration.prototype[Symbol.iterator] = function () { return this._read().map(function (d) { return d.name; }).values(); };
  setTag(CSSStyleDeclaration, 'CSSStyleDeclaration');
  global.CSSStyleDeclaration = CSSStyleDeclaration;
  // Proxy so unknown properties (custom props, anything not in CSS_PROPS) still work.
  function styleDecl(el, text) {
    var s = Object.create(CSSStyleDeclaration.prototype);
    Object.defineProperty(s, '_el', { value: el || null });
    Object.defineProperty(s, '_text', { value: text || '', writable: true });
    return new Proxy(s, {
      get: function (t, k) {
        if (typeof k === 'string' && !(k in t)) {
          if (/^\d+$/.test(k)) return t.item(+k);
          if (/^[a-zA-Z-]+$/.test(k)) return t.getPropertyValue(k.indexOf('-') >= 0 ? k : kebab(k));
        }
        return t[k];
      },
      set: function (t, k, v) {
        if (typeof k === 'string' && !(k in t) && /^[a-zA-Z-]+$/.test(k)) { t.setProperty(k.indexOf('-') >= 0 ? k : kebab(k), v); return true; }
        t[k] = v; return true;
      }
    });
  }

  // ---- dataset: Proxy over data-* attributes -------------------------------
  function dataName(prop) { return 'data-' + String(prop).replace(/[A-Z]/g, function (c) { return '-' + c.toLowerCase(); }); }
  function dataProp(attr) { return camel(attr.slice(5)); }
  function dataset(el) {
    var target = Object.create(global.DOMStringMap.prototype);
    return new Proxy(target, {
      get: function (t, k) {
        if (typeof k !== 'string') return t[k];
        var v = sw.getAttr(el[ID], dataName(k));
        return v === null || v === undefined ? (k in t ? t[k] : undefined) : v;
      },
      set: function (t, k, v) { if (typeof k === 'string') el.setAttribute(dataName(k), String(v)); return true; },
      has: function (t, k) { return typeof k === 'string' && sw.hasAttr(el[ID], dataName(k)); },
      deleteProperty: function (t, k) { if (typeof k === 'string') el.removeAttribute(dataName(k)); return true; },
      ownKeys: function () { return (sw.attrNames(el[ID]) || []).filter(function (n) { return n.indexOf('data-') === 0; }).map(dataProp); },
      getOwnPropertyDescriptor: function (t, k) {
        if (typeof k !== 'string') return undefined;
        var v = sw.getAttr(el[ID], dataName(k));
        return v === null || v === undefined ? undefined : { value: v, enumerable: true, configurable: true, writable: true };
      }
    });
  }
  function DOMStringMap() { illegal(); }
  setTag(DOMStringMap, 'DOMStringMap'); global.DOMStringMap = DOMStringMap;

  // ==== 5. Nodes ===========================================================
  var XHTML_NS = 'http://www.w3.org/1999/xhtml', SVG_NS = 'http://www.w3.org/2000/svg', MATHML_NS = 'http://www.w3.org/1998/Math/MathML';
  var NS = Symbol('namespace'), ORIG_TAG = Symbol('origTag'), FRAG = Symbol('fragmentParent'), SLOT = Symbol('slots');
  function slots(w) { var s = w[SLOT]; if (!s) { s = {}; Object.defineProperty(w, SLOT, { value: s }); } return s; }
  // structMemo caches a value derived only from tree shape (e.g. document.body) on obj until the next structural
  // mutation (sw.treeGen) or a change of obj._detachedRoot; attribute and style writes keep the cached value.
  function structMemo(obj, key, fn) {
    if (!host('treeGen')) return fn();
    var s = slots(obj), g = sw.treeGen(), c = s[key];
    if (c && c.g === g && c.r === obj._detachedRoot) return c.v;
    var v = fn(); s[key] = { g: g, r: obj._detachedRoot, v: v }; return v;
  }

  // live Range objects follow the tree (edit_prelude.js turns __swRangesLive on when the first one is made)
  function rn(kind, a, b, c, d) { if (global.__swRangesLive) global.__swRangeNotify(kind, a, b, c, d); }
  // --- tree mutation helpers (every write funnels through here) -----------
  function hostParentOf(node) { return node[ID] > 0 ? sw.parent(node[ID]) : 0; }
  function detach(node) {
    // remove `node` from wherever it is (host parent or JS fragment); returns old parent wrapper
    if (node[FRAG]) { var f = node[FRAG]; var i = f._kids.indexOf(node); if (i >= 0) f._kids.splice(i, 1); node[FRAG] = null; return f; }
    var p = hostParentOf(node);
    if (p) { var pw = wrap(p); sw.removeChild(p, node[ID]); return pw; }
    return null;
  }
  function wasConnected(node) { return node[ID] > 0 && sw.attached(node[ID]); }
  function insertNode(parent, child, ref, what) {
    requireNode(child, what);
    if (ref !== null && ref !== undefined && !isNodeObj(ref)) throw new TypeError("Failed to execute '" + what + "' on 'Node': parameter 2 is not of type 'Node'.");
    if (ref && (ref.parentNode !== parent)) throw DOMException("Failed to execute '" + what + "' on 'Node': The node before which the new node is to be inserted is not a child of this node.", 'NotFoundError');
    if (child === parent || (child.nodeType === 1 && child.contains(parent))) throw DOMException("Failed to execute '" + what + "' on 'Node': The new child element contains the parent.", 'HierarchyRequestError');
    if (child.nodeType === 11) {
      var kids = child._kids.slice();
      for (var i = 0; i < kids.length; i++) insertNode(parent, kids[i], ref, what);
      return child;
    }
    if (parent.nodeType === 11) {
      if (ref === child) return child;
      detach(child);
      var at = ref ? parent._kids.indexOf(ref) : parent._kids.length;
      parent._kids.splice(at < 0 ? parent._kids.length : at, 0, child);
      child[FRAG] = parent;
      return child;
    }
    if (parent.nodeType !== 1 && parent.nodeType !== 9) throw DOMException("Failed to execute '" + what + "' on 'Node': This node type does not support this method.", 'HierarchyRequestError');
    if (ref === child) return child;
    var oldParent = child[FRAG] ? detach(child) : wrap(hostParentOf(child));
    var connectedBefore = wasConnected(child);
    var prev = ref ? ref.previousSibling : parent.lastChild;
    if (global.__swRangesLive && oldParent && oldParent.nodeType !== 11 && !child[FRAG]) { var oi = oldParent.childNodes.length ? Array.prototype.indexOf.call(oldParent.childNodes, child) : -1; if (oi >= 0) rn('remove', child, oldParent, oi); }
    if (ref) sw.insertBefore(parent[ID], child[ID], ref[ID]); else sw.appendChild(parent[ID], child[ID]);
    if (global.__swRangesLive) rn('insert', parent, ref ? Array.prototype.indexOf.call(parent.childNodes, child) : parent.childNodes.length - 1, 1);
    if (oldParent && oldParent !== parent && oldParent.nodeType !== 11) recordMutation(oldParent, 'childList', { removedNodes: [child] });
    recordMutation(parent, 'childList', { addedNodes: [child], previousSibling: prev, nextSibling: ref || null });
    hostMutation('insert', child[ID]);
    var connectedAfter = wasConnected(child);
    if (!connectedBefore && connectedAfter) { ceConnected(child); afterConnect(child); }
    else if (connectedBefore && !connectedAfter) ceDisconnected(child);
    return child;
  }
  function removeNode(parent, child, what) {
    requireNode(child, what);
    if (child.parentNode !== parent) throw DOMException("Failed to execute '" + what + "' on 'Node': The node to be removed is not a child of this node.", 'NotFoundError');
    if (parent.nodeType === 11) { detach(child); return child; }
    var prev = child.previousSibling, next = child.nextSibling;
    var connected = wasConnected(child);
    if (connected && activeElement && (activeElement === child || child.contains(activeElement))) activeElement = null;
    var ri = global.__swRangesLive ? Array.prototype.indexOf.call(parent.childNodes, child) : -1;
    sw.removeChild(parent[ID], child[ID]);
    if (ri >= 0) rn('remove', child, parent, ri);
    recordMutation(parent, 'childList', { removedNodes: [child], previousSibling: prev, nextSibling: next });
    hostMutation('remove', child[ID]);
    if (connected) ceDisconnected(child);
    return child;
  }
  function afterConnect(node) {
    // image/script load events once attached; sweef has no network loader for scripts here
    if (node.nodeType === 1 && (node.localName === 'img' || node.localName === 'link')) scheduleLoadEvent(node);
  }
  function scheduleLoadEvent(node) {
    if (slots(node).loadScheduled) return;
    slots(node).loadScheduled = true;
    setTimeoutImpl(function () {
      slots(node).loadScheduled = false;
      if (node.localName === 'img') {
        var src = node.getAttribute('src');
        if (!src) return;
        // A real request decides load or error and the natural size (imgload_prelude.js). Without it,
        // every image in the document reported load, so onerror fallbacks never ran.
        if (typeof global.__swImgStart === 'function' && global.__swImgStart(node, src)) return;
      }
      fire(node, 'load', { bubbles: false, cancelable: false });
    }, [], 0, false);
  }
  function toNodes(args) {
    var out = [];
    for (var i = 0; i < args.length; i++) out.push(isNodeObj(args[i]) ? args[i] : documentWrapper.createTextNode(toStr(args[i])));
    if (out.length === 1) return out[0];
    var f = documentWrapper.createDocumentFragment();
    out.forEach(function (n) { f.appendChild(n); });
    return f;
  }

  // --- Node ---------------------------------------------------------------
  var Node = makeClass('Node', EventTarget);
  var NODE_CONSTS = { ELEMENT_NODE: 1, ATTRIBUTE_NODE: 2, TEXT_NODE: 3, CDATA_SECTION_NODE: 4, ENTITY_REFERENCE_NODE: 5, ENTITY_NODE: 6, PROCESSING_INSTRUCTION_NODE: 7, COMMENT_NODE: 8, DOCUMENT_NODE: 9, DOCUMENT_TYPE_NODE: 10, DOCUMENT_FRAGMENT_NODE: 11, NOTATION_NODE: 12, DOCUMENT_POSITION_DISCONNECTED: 1, DOCUMENT_POSITION_PRECEDING: 2, DOCUMENT_POSITION_FOLLOWING: 4, DOCUMENT_POSITION_CONTAINS: 8, DOCUMENT_POSITION_CONTAINED_BY: 16, DOCUMENT_POSITION_IMPLEMENTATION_SPECIFIC: 32 };
  for (var nc in NODE_CONSTS) { constant(Node, nc, NODE_CONSTS[nc]); constant(Node.prototype, nc, NODE_CONSTS[nc]); }
  // A DocumentFragment is a JS-only container (negative id, children in _kids). The Node.prototype accessors below have to
  // work on one too: scripts such as the Shady DOM polyfill capture Node.prototype's descriptors and call them on any node.
  function isFrag(n) { return n[ID] < 0; }
  getters(Node.prototype, {
    nodeType: { get: function () { return isFrag(this) ? 11 : sw.nodeType(this[ID]); } },
    nodeName: { get: function () { var t = this.nodeType; return t === 11 ? '#document-fragment' : t === 1 ? this.tagName : t === 3 ? '#text' : t === 8 ? '#comment' : t === 9 ? '#document' : t === 10 ? (sw.tagName(this[ID]) || 'html') : '#node'; } },
    parentNode: { get: function () { return isFrag(this) ? null : this[FRAG] || wrap(sw.parent(this[ID])); } },
    parentElement: { get: function () { var p = this.parentNode; return p && p.nodeType === 1 ? p : null; } },
    childNodes: { get: function () { var self = this; var s = slots(this); return s.childNodes || (s.childNodes = isFrag(this) ? liveList(NodeList, function () { return self._kids.slice(); }) : liveList(NodeList, treeCached(function () { return wrapAll(sw.children(self[ID])); }))); } },
    firstChild: { get: function () { return isFrag(this) ? this._kids[0] || null : wrap(sw.firstChild(this[ID])); } },
    lastChild: { get: function () { return isFrag(this) ? this._kids[this._kids.length - 1] || null : wrap(sw.lastChild(this[ID])); } },
    nextSibling: { get: function () { if (isFrag(this)) return null; if (this[FRAG]) { var k = this[FRAG]._kids; return k[k.indexOf(this) + 1] || null; } return wrap(sw.nextSibling(this[ID])); } },
    previousSibling: { get: function () { if (isFrag(this)) return null; if (this[FRAG]) { var k = this[FRAG]._kids; return k[k.indexOf(this) - 1] || null; } return wrap(sw.prevSibling(this[ID])); } },
    ownerDocument: { get: function () { return this.nodeType === 9 ? null : sawTemplate && inTemplateContent(this) ? templateOwnerDocument() : documentWrapper; } },
    isConnected: { get: function () { return this[ID] > 0 && sw.attached(this[ID]); } },
    baseURI: { get: function () { return currentURL(); } },
    nodeValue: {
      get: function () { var t = this.nodeType; return t === 3 || t === 8 ? sw.nodeValue(this[ID]) : null; },
      set: function (v) { var t = this.nodeType; if (t === 3 || t === 8) setCharacterData(this, toStr(v)); }
    },
    textContent: {
      get: function () { var t = this.nodeType; if (t === 9 || t === 10) return null; if (t === 3 || t === 8) return sw.nodeValue(this[ID]); if (t === 11) return this._kids.map(function (k) { return k.nodeType === 8 ? '' : k.textContent; }).join(''); return sw.textContent(this[ID]); },
      set: function (v) {
        var t = this.nodeType; v = v === null || v === undefined ? '' : String(v);
        if (t === 11) { while (this._kids.length) this.removeChild(this._kids[0]); if (v !== '') this.appendChild(documentWrapper.createTextNode(v)); return; }
        if (t === 3 || t === 8) { setCharacterData(this, v); return; }
        if (t !== 1 && t !== 9) return;
        var removed = wrapAll(sw.children(this[ID]));
        removed.forEach(function (n) { if (wasConnected(n)) ceDisconnected(n); });
        sw.setTextContent(this[ID], v);
        if (global.__swRangesLive) { var self0 = this; removed.forEach(function (n) { rn('remove', n, self0, 0); }); }
        var added = wrapAll(sw.children(this[ID]));
        if (global.__swRangesLive) rn('insert', this, 0, added.length);
        recordMutation(this, 'childList', { addedNodes: added, removedNodes: removed });
        hostMutation('text', this[ID]);
      }
    }
  });
  function setCharacterData(node, v, off, count, newLen) {
    var old = sw.nodeValue(node[ID]);
    sw.setNodeValue(node[ID], v);
    if (global.__swRangesLive && off !== -1) { if (off === undefined) rn('data', node, 0, old.length, v.length); else rn('data', node, off, count, newLen); }
    recordMutation(node, 'characterData', { oldValue: old });
    hostMutation('data', node[ID]);
  }
  methods(Node.prototype, {
    appendChild: function (child) { return insertNode(this, child, null, 'appendChild'); },
    insertBefore: function (child, ref) { return insertNode(this, child, ref === undefined ? null : ref, 'insertBefore'); },
    removeChild: function (child) { return removeNode(this, child, 'removeChild'); },
    replaceChild: function (newChild, oldChild) {
      requireNode(newChild, 'replaceChild'); requireNode(oldChild, 'replaceChild');
      if (oldChild.parentNode !== this) throw DOMException("Failed to execute 'replaceChild' on 'Node': The node to be replaced is not a child of this node.", 'NotFoundError');
      var next = oldChild.nextSibling;
      removeNode(this, oldChild, 'replaceChild');
      insertNode(this, newChild, next === newChild ? newChild.nextSibling : next, 'replaceChild');
      return oldChild;
    },
    hasChildNodes: function () { return this.nodeType === 11 ? this._kids.length > 0 : sw.children(this[ID]).length > 0; },
    cloneNode: function (deep) { if (this.nodeType === 11) { var f = documentWrapper.createDocumentFragment(); if (deep) this._kids.forEach(function (k) { f.appendChild(k.cloneNode(true)); }); return f; } var id = sw.cloneNode(this[ID], !!deep); var w = wrap(id); if (this[NS] && w) w[NS] = this[NS]; return w; },
    contains: function (other) {
      if (!isNodeObj(other)) return false;
      for (var n = other; n; n = n.parentNode) if (n === this) return true;
      return false;
    },
    compareDocumentPosition: function (other) {
      if (other === this) return 0;
      if (!isNodeObj(other)) throw new TypeError("parameter 1 is not of type 'Node'.");
      if (this.contains(other)) return 16 | 4;
      if (other.contains(this)) return 8 | 2;
      var a = ancestorsOf(this), b = ancestorsOf(other);
      if (a[0] !== b[0]) return 1 | 32 | (idOf(this) < idOf(other) ? 4 : 2);
      var i = 0; while (a[i] === b[i]) i++;
      var sibs = itemsOf(a[i - 1].childNodes);
      return sibs.indexOf(a[i]) < sibs.indexOf(b[i]) ? 4 : 2;
    },
    getRootNode: function () { var n = this; while (n.parentNode) n = n.parentNode; return n; },
    isSameNode: function (o) { return o === this; },
    isEqualNode: function (o) { return !!o && o.nodeType === this.nodeType && (this.nodeType === 1 ? this.outerHTML === o.outerHTML : this.nodeValue === o.nodeValue); },
    isDefaultNamespace: function (ns) { return ns === XHTML_NS; },
    lookupNamespaceURI: function (prefix) { return prefix ? null : this.namespaceURI || null; },
    lookupPrefix: function () { return null; },
    normalize: function () {
      var kids = wrapAll(sw.children(this[ID]));
      for (var i = 0; i < kids.length; i++) {
        var k = kids[i];
        if (k.nodeType === 3) {
          if (k.data === '') { this.removeChild(k); continue; }
          while (kids[i + 1] && kids[i + 1].nodeType === 3) { k.appendData(kids[i + 1].data); this.removeChild(kids[i + 1]); kids.splice(i + 1, 1); }
        } else if (k.nodeType === 1) k.normalize();
      }
    },
    toString: function () { return '[object ' + this[Symbol.toStringTag] + ']'; }
  });
  function ancestorsOf(n) { var a = []; for (; n; n = n.parentNode) a.unshift(n); return a; }

  // --- Element ------------------------------------------------------------
  var Element = makeClass('Element', Node);
  function childElements(id) { var ids = sw.children(id), out = []; for (var i = 0; i < ids.length; i++) if (sw.nodeType(ids[i]) === 1) out.push(wrap(ids[i])); return out; }
  function nextElement(id, dir) { for (var s = sw[dir](id); s; s = sw[dir](s)) if (sw.nodeType(s) === 1) return wrap(s); return null; }
  var ParentNodeGetters = {
    children: { get: function () { var self = this; var s = slots(this); return s.children || (s.children = liveList(HTMLCollection, self.nodeType === 11 ? function () { return self._kids.filter(function (k) { return k.nodeType === 1; }); } : treeCached(function () { return childElements(self[ID]); }))); } },
    firstElementChild: { get: function () { return this.children[0] || null; } },
    lastElementChild: { get: function () { var c = this.children; return c[c.length - 1] || null; } },
    childElementCount: { get: function () { return this.children.length; } }
  };
  var ParentNodeMethods = {
    append: function () { this.appendChild(toNodes(arguments)); },
    prepend: function () { this.insertBefore(toNodes(arguments), this.firstChild); },
    replaceChildren: function () { while (this.firstChild) this.removeChild(this.firstChild); if (arguments.length) this.appendChild(toNodes(arguments)); },
    querySelector: function (sel) { return queryOne(this, sel); },
    querySelectorAll: function (sel) { return nodeListOf(queryAll(this, sel)); },
    getElementsByTagName: function (tag) { var self = this; tag = String(tag).toLowerCase(); return liveList(HTMLCollection, treeCached(function () { return wrapAll(sw.elementsByTag(queryRoot(self), tag)); })); },
    getElementsByTagNameNS: function (ns, tag) { return this.getElementsByTagName(tag); },
    getElementsByClassName: function (cls) { var self = this; cls = String(cls); return liveList(HTMLCollection, genCached(function () { return wrapAll(sw.elementsByClass(queryRoot(self), cls)); })); }
  };
  var ChildNodeMethods = {
    remove: function () { var p = this.parentNode; if (p) p.removeChild(this); },
    before: function () { var p = this.parentNode; if (p) p.insertBefore(toNodes(arguments), this); },
    after: function () { var p = this.parentNode; if (p) p.insertBefore(toNodes(arguments), this.nextSibling); },
    replaceWith: function () { var p = this.parentNode; if (p) { var n = toNodes(arguments); var next = this.nextSibling; p.removeChild(this); p.insertBefore(n, next); } }
  };
  function queryRoot(node) { return node.nodeType === 9 ? 0 : node[ID]; }
  function selectorError(sel, e) { if (e && e.name === 'SyntaxError') throw e; throw DOMException("Failed to execute 'querySelector' on 'Document': '" + sel + "' is not a valid selector.", 'SyntaxError'); }
  function queryOne(node, sel) {
    sel = toStr(sel);
    if (node._detachedRoot) node = node._detachedRoot;
    if (node.nodeType === 11) { var all = queryAll(node, sel); return all[0] || null; }
    try { return wrap(sw.querySelector(queryRoot(node), sel)); } catch (e) { selectorError(sel, e); }
  }
  function queryAll(node, sel) {
    sel = toStr(sel);
    if (node._detachedRoot) node = node._detachedRoot;
    if (node.nodeType === 11) {
      var out = [];
      node._kids.forEach(function (k) { if (k.nodeType !== 1) return; if (k.matches(sel)) out.push(k); out.push.apply(out, queryAll(k, sel)); });
      return out;
    }
    try { return wrapAll(sw.querySelectorAll(queryRoot(node), sel)); } catch (e) { selectorError(sel, e); }
  }
  getters(Element.prototype, ParentNodeGetters);
  methods(Element.prototype, ParentNodeMethods);
  methods(Element.prototype, ChildNodeMethods);
  getters(Element.prototype, {
    tagName: { get: function () { var t = this[ORIG_TAG] || sw.tagName(this[ID]); return this[NS] && this[NS] !== XHTML_NS ? t : t.toUpperCase(); } },
    localName: { get: function () { return this[ORIG_TAG] || sw.tagName(this[ID]); } },
    namespaceURI: { get: function () { return this[NS] || (this instanceof global.SVGElement ? SVG_NS : XHTML_NS); } },
    prefix: { get: function () { return null; } },
    id: { get: function () { return sw.getAttr(this[ID], 'id') || ''; }, set: function (v) { this.setAttribute('id', toStr(v)); } },
    className: { get: function () { return sw.getAttr(this[ID], 'class') || ''; }, set: function (v) { this.setAttribute('class', toStr(v)); } },
    classList: { get: function () { var s = slots(this); return s.classList || (s.classList = tokenList(this, 'class')); } },
    part: { get: function () { var s = slots(this); return s.part || (s.part = tokenList(this, 'part')); } },
    slot: { get: function () { return sw.getAttr(this[ID], 'slot') || ''; }, set: function (v) { this.setAttribute('slot', toStr(v)); } },
    attributes: { get: function () { var s = slots(this); return s.attributes || (s.attributes = namedNodeMap(this)); } },
    innerHTML: {
      get: function () { return sw.innerHTML(this[ID]); },
      set: function (v) {
        v = v === null || v === undefined ? '' : String(v);
        // Fast path: with no custom-element definitions, page MutationObservers, live Ranges or opened popovers, the
        // per-node hooks below are all no-ops (fresh parser output has no wrappers or state), so skip wrapping every
        // old and new child. Markup that can create a top-level <img>/<link> (whose load events afterConnect
        // schedules), or media markup the platform media observer acts on, takes the full path.
        if (ceDefinitions.size === 0 && onlyMediaObservers() && !global.__swRangesLive && !popoverEverOpened &&
            !/<(img|image|link|video|audio|track|source)\b/i.test(v) && this.localName !== 'video' && this.localName !== 'audio') {
          sw.setInnerHTML(this[ID], v);
          hostMutation('innerHTML', this[ID]);
          return;
        }
        var removed = wrapAll(sw.children(this[ID]));
        removed.forEach(function (n) { if (wasConnected(n)) ceDisconnected(n); });
        sw.setInnerHTML(this[ID], v);
        if (global.__swRangesLive) { var self1 = this; removed.forEach(function (n) { rn('remove', n, self1, 0); }); }
        var added = wrapAll(sw.children(this[ID]));
        if (global.__swRangesLive) rn('insert', this, 0, added.length);
        recordMutation(this, 'childList', { addedNodes: added, removedNodes: removed });
        hostMutation('innerHTML', this[ID]);
        if (wasConnected(this)) added.forEach(function (n) { ceConnected(n); afterConnect(n); });
      }
    },
    outerHTML: {
      get: function () { return sw.outerHTML(this[ID]); },
      set: function (v) { var p = this.parentNode; if (!p || p.nodeType !== 1) throw DOMException("Failed to set the 'outerHTML' property on 'Element': This element has no parent node.", 'NoModificationAllowedError'); this.insertAdjacentHTML('afterend', toStr(v)); p.removeChild(this); }
    },
    innerText: { get: function () { return host('innerText') ? sw.innerText(this[ID]) : sw.textContent(this[ID]); }, set: function (v) { this.textContent = v; } },
    outerText: { get: function () { return host('innerText') ? sw.innerText(this[ID]) : sw.textContent(this[ID]); }, set: function (v) { var t = documentWrapper.createTextNode(toStr(v)); this.replaceWith(t); } },
    nextElementSibling: { get: function () { if (this[FRAG]) { var k = this[FRAG]._kids; for (var i = k.indexOf(this) + 1; i < k.length; i++) if (k[i].nodeType === 1) return k[i]; return null; } return nextElement(this[ID], 'nextSibling'); } },
    previousElementSibling: { get: function () { if (this[FRAG]) { var k = this[FRAG]._kids; for (var i = k.indexOf(this) - 1; i >= 0; i--) if (k[i].nodeType === 1) return k[i]; return null; } return nextElement(this[ID], 'prevSibling'); } },
    shadowRoot: { get: function () { return slots(this).shadowRoot || null; } },
    assignedSlot: { get: function () { return null; } },
    // geometry from the Go side
    clientWidth: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.cw : b.width); } },
    clientHeight: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.ch : b.height); } },
    clientTop: { get: function () { return 0; } }, clientLeft: { get: function () { return 0; } },
    scrollWidth: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.sw : b.width); } },
    scrollHeight: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.sh : b.height); } },
    scrollTop: { get: function () { return scrollGet(this, 1); }, set: function (v) { scrollSetPos(this, undefined, +v || 0, 'instant'); } },
    scrollLeft: { get: function () { return scrollGet(this, 0); }, set: function (v) { scrollSetPos(this, +v || 0, undefined, 'instant'); } }
  });
  methods(Element.prototype, {
    getAttribute: function (name) { var v = sw.getAttr(this[ID], attrName(this, name)); return v === undefined ? null : v; },
    getAttributeNS: function (ns, name) { return this.getAttribute(name); },
    setAttribute: function (name, value) {
      if (arguments.length < 2) throw new TypeError("Failed to execute 'setAttribute' on 'Element': 2 arguments required, but only " + arguments.length + " present.");
      name = attrName(this, name);
      if (!/^[^\s"'>\/=\u0000-\u001f]+$/.test(name)) throw DOMException("Failed to execute 'setAttribute' on 'Element': '" + name + "' is not a valid attribute name.", 'InvalidCharacterError');
      value = toStr(value);
      var old = sw.getAttr(this[ID], name);
      if (old === value) return;
      sw.setAttr(this[ID], name, value);
      recordMutation(this, 'attributes', { attributeName: name, oldValue: old === undefined ? null : old });
      hostMutation('attr', this[ID]);
      ceAttributeChanged(this, name, old === undefined ? null : old, value);
    },
    setAttributeNS: function (ns, name, value) { this.setAttribute(name.indexOf(':') >= 0 && ns !== 'http://www.w3.org/1999/xlink' ? name.split(':')[1] : name, value); },
    removeAttribute: function (name) {
      name = attrName(this, name);
      if (!sw.hasAttr(this[ID], name)) return;
      var old = sw.getAttr(this[ID], name);
      sw.removeAttr(this[ID], name);
      recordMutation(this, 'attributes', { attributeName: name, oldValue: old });
      hostMutation('attr', this[ID]);
      ceAttributeChanged(this, name, old, null);
    },
    removeAttributeNS: function (ns, name) { this.removeAttribute(name); },
    hasAttribute: function (name) { return !!sw.hasAttr(this[ID], attrName(this, name)); },
    hasAttributeNS: function (ns, name) { return this.hasAttribute(name); },
    hasAttributes: function () { return (sw.attrNames(this[ID]) || []).length > 0; },
    getAttributeNames: function () { return (sw.attrNames(this[ID]) || []).slice(); },
    toggleAttribute: function (name, force) { var has = this.hasAttribute(name); if (force === undefined) force = !has; if (force && !has) this.setAttribute(name, ''); else if (!force && has) this.removeAttribute(name); return !!force; },
    getAttributeNode: function (name) { return this.attributes.getNamedItem(name); },
    setAttributeNode: function (attr) { this.setAttribute(attr.name, attr.value); return null; },
    removeAttributeNode: function (attr) { this.removeAttribute(attr.name); return attr; },
    matches: function (sel) { sel = toStr(sel); try { return !!sw.matches(this[ID], sel); } catch (e) { selectorError(sel, e); } },
    webkitMatchesSelector: function (sel) { return this.matches(sel); },
    msMatchesSelector: function (sel) { return this.matches(sel); },
    closest: function (sel) { sel = toStr(sel); for (var n = this; n && n.nodeType === 1; n = n.parentNode) if (n.matches(sel)) return n; return null; },
    insertAdjacentHTML: function (position, html) {
      position = String(position).toLowerCase();
      if (['beforebegin', 'afterbegin', 'beforeend', 'afterend'].indexOf(position) < 0) throw DOMException("Failed to execute 'insertAdjacentHTML' on 'Element': The value provided ('" + position + "') is not one of 'beforeBegin', 'afterBegin', 'beforeEnd', or 'afterEnd'.", 'SyntaxError');
      var container = position === 'beforebegin' || position === 'afterend' ? this.parentNode : this;
      if (!container) return;
      var before = container.nodeType !== 11 ? wrapAll(sw.children(container[ID])) : null;
      sw.insertAdjacentHTML(this[ID], position, toStr(html));
      if (before) {
        var after = wrapAll(sw.children(container[ID]));
        var added = after.filter(function (n) { return before.indexOf(n) < 0; });
        if (added.length) { recordMutation(container, 'childList', { addedNodes: added }); if (wasConnected(container)) added.forEach(function (n) { ceConnected(n); afterConnect(n); }); }
      }
      hostMutation('insertAdjacentHTML', this[ID]);
    },
    insertAdjacentElement: function (position, el) { return insertAdjacent(this, position, el); },
    insertAdjacentText: function (position, text) { insertAdjacent(this, position, documentWrapper.createTextNode(toStr(text))); },
    getBoundingClientRect: function () { return domRect(hostBox(this[ID])); },
    getClientRects: function () { var hb = hostBox(this[ID]); var list = hb.precise && hb.frags && hb.frags.length ? hb.frags.map(function (f) { var p = scrollPos(); return new DOMRect(f[0] - p[0], f[1] - p[1], f[2], f[3]); }) : [this.getBoundingClientRect()]; list.item = function (i) { return list[i] || null; }; return list; },
    scrollIntoView: function (arg) { scrollIntoViewImpl(this, arg); }, scrollIntoViewIfNeeded: function (center) { scrollIntoViewImpl(this, { block: center === false ? 'nearest' : 'center', inline: 'nearest' }); },
    scroll: function (a, b) { scrollToArgs(this, a, b, false); }, scrollTo: function (a, b) { scrollToArgs(this, a, b, false); }, scrollBy: function (a, b) { scrollToArgs(this, a, b, true); },
    attachShadow: function (init) { var s = slots(this); if (s.shadowRoot) throw DOMException('Shadow root cannot be created on a host which already hosts a shadow tree.', 'NotSupportedError'); s.shadowRoot = this; return this; },
    requestFullscreen: function () { return Promise.resolve(); },
    requestPointerLock: function () { }, setPointerCapture: function () { }, releasePointerCapture: function () { }, hasPointerCapture: function () { return false; },
    animate: function (keyframes, options) { return waapiAnimate(this, keyframes, options); },
    getAnimations: function () { var el = this; return waapiAll.filter(function (a) { return a.effect && a.effect.target === el && a.playState !== 'finished' && a.playState !== 'idle'; }); },
    checkVisibility: function () { return this.isConnected; }
  });
  // ---- Web Animations (Element.animate) ----
  // A working subset: keyframes (array or property-indexed form, offsets), duration, delay, iterations,
  // direction, fill and linear timing, run in real time on animation frames. Values are written to the
  // element's inline style while the animation is in effect (the value the page would compute) and the
  // previous inline value comes back when it stops having an effect (fill none / cancel). Numbers inside
  // values with the same shape interpolate (opacity, lengths, transforms with the same functions, rgb()
  // colours); other values switch at the midpoint. finish/cancel events and the finished/ready promises
  // follow the spec closely enough for animation libraries (Framer Motion commits its final values when an
  // animation finishes: with the old stub nothing ever finished, and content animated in from opacity 0
  // stayed invisible in screenshots).
  var waapiAll = [];
  function waapiKebab(p) { return p === 'cssFloat' ? 'float' : p.indexOf('--') === 0 ? p : p.replace(/[A-Z]/g, function (c) { return '-' + c.toLowerCase(); }); }
  function waapiFrames(kf) {
    var frames = [];
    if (Array.isArray(kf)) frames = kf.map(function (f) { return Object.assign({}, f); });
    else if (kf && typeof kf === 'object') {
      var props = Object.keys(kf).filter(function (k) { return k !== 'offset' && k !== 'easing' && k !== 'composite'; }), n = 0;
      props.forEach(function (k) { n = Math.max(n, Array.isArray(kf[k]) ? kf[k].length : 1); });
      for (var i = 0; i < n; i++) {
        var f = {};
        props.forEach(function (k) { var v = Array.isArray(kf[k]) ? kf[k] : [kf[k]]; if (i < v.length) f[k] = v[i]; });
        if (Array.isArray(kf.offset) && kf.offset[i] != null) f.offset = kf.offset[i];
        frames.push(f);
      }
    }
    var m = frames.length;
    frames.forEach(function (f, i) { if (f.offset == null) f.offset = m <= 1 ? 1 : i / (m - 1); });
    return frames;
  }
  function waapiLerp(a, b, t) {
    a = String(a); b = String(b);
    if (t <= 0) return a; if (t >= 1) return b;
    var na = a.match(/-?\d*\.?\d+(e-?\d+)?/gi) || [], nb = b.match(/-?\d*\.?\d+(e-?\d+)?/gi) || [];
    var sa = a.replace(/-?\d*\.?\d+(e-?\d+)?/gi, '#'), sb = b.replace(/-?\d*\.?\d+(e-?\d+)?/gi, '#');
    if (sa !== sb || na.length !== nb.length || !na.length) return t < 0.5 ? a : b;
    var i = 0;
    return sa.replace(/#/g, function () { var x = +na[i], y = +nb[i]; i++; var v = x + (y - x) * t; return String(Math.round(v * 10000) / 10000); });
  }
  // The animated values of an element live in the host attribute data-sw-anim, which the style engine applies
  // after the element's normal declarations, inline style included (the animation origin of the cascade):
  // page writes to inline style never fight an animation, and getComputedStyle shows the animated value.
  function waapiEffect(el, anim, vals) {
    var sl = slots(el), list = sl.anims || (sl.anims = []), i;
    for (i = 0; i < list.length && list[i].a !== anim; i++) {}
    if (vals) { if (i < list.length) list[i].v = vals; else list.push({ a: anim, v: vals }); } else if (i < list.length) list.splice(i, 1);
    var merged = {}; list.forEach(function (e) { Object.keys(e.v).forEach(function (k) { merged[k] = e.v[k]; }); });
    var text = Object.keys(merged).map(function (k) { return k + ': ' + merged[k]; }).join('; ');
    if (text === (sl.animText || '')) return;
    sl.animText = text;
    try { if (text) sw.setAttr(el[ID], 'data-sw-anim', text); else sw.removeAttr(el[ID], 'data-sw-anim'); } catch (e) { /* detached */ }
  }
  function waapiAnimate(el, keyframes, options) {
    var o = typeof options === 'number' ? { duration: options } : Object.assign({}, options || {});
    var dur = o.duration === 'auto' || o.duration == null ? 0 : Math.max(0, +o.duration || 0);
    var delay = +o.delay || 0, endDelay = +o.endDelay || 0, iters = o.iterations == null ? 1 : +o.iterations;
    if (!(iters >= 0)) iters = 1;
    var fill = o.fill || 'auto', dir = o.direction || 'normal';
    var frames = waapiFrames(keyframes), props = {};
    frames.forEach(function (f) { Object.keys(f).forEach(function (k) { if (k !== 'offset' && k !== 'easing' && k !== 'composite') props[k] = 1; }); });
    props = Object.keys(props);
    var resolveFin, rejectFin, rafId = 0, holdTime = null, rate = +o.playbackRate || 1;
    var anim = {
      id: o.id || '', playState: 'running', pending: false, replaceState: 'active', playbackRate: rate, timeline: documentWrapper.timeline || null,
      startTime: nowMs(), onfinish: null, oncancel: null, onremove: null, _listeners: {},
      effect: {
        target: el, pseudoElement: o.pseudoElement || null, composite: o.composite || 'replace',
        getKeyframes: function () { return frames.map(function (f) { return Object.assign({ computedOffset: f.offset, easing: 'linear', composite: 'auto' }, f); }); },
        setKeyframes: function (k) { frames = waapiFrames(k); },
        getTiming: function () { return { delay: delay, endDelay: endDelay, duration: dur, iterations: iters, direction: dir, fill: fill, easing: o.easing || 'linear' }; },
        getComputedTiming: function () { var ct = local(); return { localTime: ct, progress: progressAt(ct), currentIteration: 0, activeDuration: dur * iters, endTime: delay + dur * iters + endDelay, duration: dur, delay: delay, iterations: iters, direction: dir, fill: fill }; },
        updateTiming: function (t) { if (t) { if (t.duration != null) dur = +t.duration; if (t.delay != null) delay = +t.delay; if (t.iterations != null) iters = +t.iterations; if (t.fill) fill = t.fill; } }
      },
      addEventListener: function (t, f) { (this._listeners[t] = this._listeners[t] || []).push(f); },
      removeEventListener: function (t, f) { var l = this._listeners[t]; if (l) { var i = l.indexOf(f); if (i >= 0) l.splice(i, 1); } },
      dispatchEvent: function (e) { var l = (this._listeners[e.type] || []).slice(); l.forEach(function (f) { try { f.call(anim, e); } catch (x) { reportError(x); } }); return true; }
    };
    anim.finished = new Promise(function (res, rej) { resolveFin = res; rejectFin = rej; });
    anim.finished.catch(function () { });
    anim.ready = Promise.resolve(anim);
    function local() { return holdTime != null ? holdTime : (nowMs() - anim.startTime) * rate; }
    function endTime() { return delay + dur * iters; }
    function progressAt(t) {
      if (t < delay) return null;
      if (!isFinite(iters) || dur <= 0 && iters > 0) { if (dur <= 0) return 1; }
      var act = Math.min(t - delay, dur * iters);
      var it = dur > 0 ? act / dur : iters, cur = Math.floor(it), p = it - cur;
      if (act >= dur * iters && iters > 0) { p = iters % 1 || 1; cur = Math.ceil(iters) - 1; }
      var rev = dir === 'reverse' || (dir === 'alternate' && cur % 2 === 1) || (dir === 'alternate-reverse' && cur % 2 === 0);
      return rev ? 1 - p : p;
    }
    function valueAt(prop, p) {
      var fs = frames.filter(function (f) { return f[prop] != null; });
      if (!fs.length) return null;
      if (fs.length === 1) { var under = el.style.getPropertyValue(waapiKebab(prop)) || getComputedStyle(el).getPropertyValue(waapiKebab(prop)); return fs[0].offset >= 1 ? waapiLerp(under, fs[0][prop], p) : fs[0][prop]; }
      if (p <= fs[0].offset) return fs[0][prop];
      for (var i = 1; i < fs.length; i++) if (p <= fs[i].offset) { var a = fs[i - 1], b = fs[i], span = b.offset - a.offset; return waapiLerp(a[prop], b[prop], span > 0 ? (p - a.offset) / span : 1); }
      return fs[fs.length - 1][prop];
    }
    var effectOn = false;
    function apply(p) {
      var vals = {};
      props.forEach(function (k) { var v = valueAt(k, p); if (v != null) vals[waapiKebab(k)] = String(v); });
      effectOn = true; waapiEffect(el, anim, vals);
    }
    function restore() { if (effectOn) { effectOn = false; waapiEffect(el, anim, null); } }
    function holdsAfter() { return fill === 'forwards' || fill === 'both'; }
    function holdsBefore() { return fill === 'backwards' || fill === 'both'; }
    function fire(type) { var e = { type: type, target: anim, currentTime: local(), timelineTime: nowMs() }; var h = anim['on' + type]; if (typeof h === 'function') { try { h.call(anim, e); } catch (x) { reportError(x); } } anim.dispatchEvent(e); }
    function finishNow() {
      if (anim.playState === 'finished') return;
      anim.playState = 'finished';
      if (holdsAfter()) apply(progressAt(endTime()) == null ? 1 : progressAt(endTime())); else restore();
      var i = waapiAll.indexOf(anim); if (i >= 0 && !holdsAfter()) waapiAll.splice(i, 1);
      resolveFin(anim);
      fire('finish');
    }
    function tick() {
      rafId = 0;
      if (anim.playState !== 'running') return;
      var t = local();
      if (isFinite(iters) && t >= endTime()) { apply(progressAt(endTime()) == null ? 1 : progressAt(endTime())); global.setTimeout(finishNow, 0); return; } // the finish event is a task after the frame, as in a browser
      var p = progressAt(t);
      if (p == null) { if (holdsBefore()) apply(progressAt(delay) || 0); } else apply(p);
      rafId = global.requestAnimationFrame(tick);
    }
    anim.play = function () { if (anim.playState === 'finished') { anim.startTime = nowMs(); holdTime = null; } else if (holdTime != null) { anim.startTime = nowMs() - holdTime / rate; holdTime = null; } anim.playState = 'running'; if (!rafId) rafId = global.requestAnimationFrame(tick); };
    anim.pause = function () { if (anim.playState === 'running') { holdTime = local(); anim.playState = 'paused'; } };
    anim.finish = function () { finishNow(); };
    anim.cancel = function () { if (anim.playState === 'idle') return; anim.playState = 'idle'; restore(); var i = waapiAll.indexOf(anim); if (i >= 0) waapiAll.splice(i, 1); var e = DOMException('The user aborted a request.', 'AbortError'); rejectFin(e); anim.finished = new Promise(function (res, rej) { resolveFin = res; rejectFin = rej; }); anim.finished.catch(function () { }); fire('cancel'); };
    anim.reverse = function () { rate = -rate; anim.playbackRate = rate; };
    anim.updatePlaybackRate = function (r) { var t = local(); rate = +r || 1; anim.playbackRate = rate; anim.startTime = nowMs() - t / rate; };
    anim.commitStyles = function () { var p = progressAt(local()); if (p == null) p = 1; props.forEach(function (k) { var v = valueAt(k, p); if (v != null) el.style.setProperty(waapiKebab(k), String(v)); }); };
    anim.persist = function () { anim.replaceState = 'persisted'; };
    Object.defineProperty(anim, 'currentTime', { get: function () { return anim.playState === 'idle' ? null : local(); }, set: function (v) { if (v == null) return; if (anim.playState === 'running') anim.startTime = nowMs() - (+v) / rate; else holdTime = +v; var p = progressAt(+v); if (p != null) apply(p); }, configurable: true });
    waapiAll.push(anim);
    var p0 = progressAt(0);
    if (p0 != null) apply(p0); else if (holdsBefore()) apply(progressAt(delay) || 0);
    if (dur * iters + delay <= 0 && isFinite(iters)) Promise.resolve().then(finishNow); // zero-length: finishes in the next microtask
    else rafId = global.requestAnimationFrame(tick);
    return anim;
  }

  function attrName(el, name) { name = toStr(name); return el[NS] && el[NS] !== XHTML_NS ? name : name.toLowerCase(); }
  function insertAdjacent(el, position, node) {
    position = String(position).toLowerCase();
    var p = el.parentNode;
    switch (position) {
      case 'beforebegin': if (!p) return null; p.insertBefore(node, el); return node;
      case 'afterbegin': el.insertBefore(node, el.firstChild); return node;
      case 'beforeend': el.appendChild(node); return node;
      case 'afterend': if (!p) return null; p.insertBefore(node, el.nextSibling); return node;
    }
    throw DOMException("The value provided ('" + position + "') is not one of 'beforeBegin', 'afterBegin', 'beforeEnd', or 'afterEnd'.", 'SyntaxError');
  }
  function DOMRect(x, y, width, height) { this.x = +x || 0; this.y = +y || 0; this.width = +width || 0; this.height = +height || 0; }
  getters(DOMRect.prototype, {
    top: { get: function () { return Math.min(this.y, this.y + this.height); } },
    left: { get: function () { return Math.min(this.x, this.x + this.width); } },
    right: { get: function () { return Math.max(this.x, this.x + this.width); } },
    bottom: { get: function () { return Math.max(this.y, this.y + this.height); } }
  });
  method(DOMRect.prototype, 'toJSON', function () { return { x: this.x, y: this.y, width: this.width, height: this.height, top: this.top, right: this.right, bottom: this.bottom, left: this.left }; });
  DOMRect.fromRect = function (r) { r = r || {}; return new DOMRect(r.x, r.y, r.width, r.height); };
  setTag(DOMRect, 'DOMRect'); global.DOMRect = DOMRect; global.DOMRectReadOnly = DOMRect;
  // ---- scrolling: elements with overflow, and the viewport (the root element)
  var pendingScroll = [], scrollTimer = null, scrollEndTimer = null;
  function isRootScroller(el) { return el === documentWrapper.documentElement || (el === documentWrapper.body && sw.quirks()); }
  function scrollGet(el, axis) {
    if (isRootScroller(el)) return scrollPos()[axis];
    var b = hostBox(el[ID]);
    return b.precise ? (axis ? b.st : b.sl) || 0 : 0;
  }
  function flushScrollEvents() {
    scrollTimer = null;
    var list = pendingScroll; pendingScroll = [];
    for (var i = 0; i < list.length; i++) {
      var t = list[i];
      if (t === documentWrapper) fire(documentWrapper, 'scroll', { bubbles: true, cancelable: false });
      else fire(t, 'scroll', { bubbles: false, cancelable: false });
    }
    if (scrollEndTimer) global.clearTimeout(scrollEndTimer);
    scrollEndTimer = global.setTimeout(function () {
      scrollEndTimer = null;
      for (var j = 0; j < list.length; j++) fire(list[j], 'scrollend', { bubbles: list[j] === documentWrapper, cancelable: false });
    }, 120);
  }
  function queueScrollEvent(target) {
    if (pendingScroll.indexOf(target) < 0) pendingScroll.push(target);
    if (scrollTimer === null) scrollTimer = global.setTimeout(flushScrollEvents, 0);
  }
  // behavior: 'smooth' | 'instant' | 'auto' (the scroll-behavior of the element decides)
  function scrollSetPos(el, x, y, behavior) {
    if (isRootScroller(el)) { windowScroll(x, y, false, behavior); return; }
    var cur = hostBox(el[ID]);
    var nx = x === undefined ? (cur.sl || 0) : x, ny = y === undefined ? (cur.st || 0) : y;
    if (!host('scrollSet')) return;
    var r = sw.scrollSet(el[ID], nx, ny);
    if (r && r[2]) queueScrollEvent(el);
  }
  function scrollToArgs(el, a, b, relative) {
    var x, y, behavior = 'auto';
    if (a !== null && typeof a === 'object') {
      x = a.left === undefined ? undefined : +a.left; y = a.top === undefined ? undefined : +a.top;
      if (a.behavior) behavior = String(a.behavior);
    } else if (arguments.length >= 3 || a !== undefined) { x = a === undefined ? undefined : +a; y = b === undefined ? undefined : +b; if (arguments.length < 4) { /* scrollTo(x, y) */ } }
    if (x !== undefined && isNaN(x)) x = 0;
    if (y !== undefined && isNaN(y)) y = 0;
    if (isRootScroller(el)) { windowScroll(x, y, relative, behavior); return; }
    var cx = scrollGet(el, 0), cy = scrollGet(el, 1);
    scrollSetPos(el, x === undefined ? undefined : (relative ? cx + x : x), y === undefined ? undefined : (relative ? cy + y : y), behavior);
  }
  // window.scrollTo / scrollBy / scroll and the root element's scrollTop: the viewport belongs to the window
  function windowScroll(x, y, relative, behavior) {
    var p = scrollPos();
    var nx = x === undefined ? p[0] : (relative ? p[0] + x : x), ny = y === undefined ? p[1] : (relative ? p[1] + y : y);
    if (!host('scrollWindow')) return;
    var applied = sw.scrollWindow(nx, ny, String(behavior || 'auto'));
    if (applied) queueScrollEvent(documentWrapper);
  }
  global.__swScrollEvent = function (target) { queueScrollEvent(target === 'document' ? documentWrapper : target); };
  function scrollWinArgs(relative) {
    return function (a, b) {
      var x, y, behavior = 'auto';
      if (a !== null && typeof a === 'object') { x = a.left === undefined ? undefined : +a.left; y = a.top === undefined ? undefined : +a.top; if (a.behavior) behavior = String(a.behavior); }
      else if (arguments.length >= 2) { x = +a; y = +b; } else if (arguments.length === 1) { throw new TypeError("Failed to execute 'scrollTo' on 'Window': parameter 1 is not of type 'ScrollToOptions'."); }
      if (x !== undefined && isNaN(x)) x = 0;
      if (y !== undefined && isNaN(y)) y = 0;
      windowScroll(x, y, relative, behavior);
    };
  }
  function scrollIntoViewImpl(el, arg) {
    var block = 'start', inline = 'nearest', behavior = 'auto';
    if (arg === false) block = 'end';
    else if (arg && typeof arg === 'object') {
      if (arg.block) block = String(arg.block); if (arg.inline) inline = String(arg.inline); if (arg.behavior) behavior = String(arg.behavior);
    }
    if (!el.isConnected) return;
    function delta(lo, hi, vlo, vhi, align) { // how far to scroll so [lo,hi] sits in [vlo,vhi] as asked
      var size = hi - lo, vsize = vhi - vlo;
      switch (align) {
        case 'start': return lo - vlo;
        case 'end': return hi - vhi;
        case 'center': return lo + size / 2 - (vlo + vsize / 2);
        default: // nearest
          if (lo >= vlo && hi <= vhi) return 0;
          if (size > vsize) return lo < vlo ? hi - vhi : lo - vlo; // bigger than the view: the nearest edge
          return lo < vlo ? lo - vlo : hi - vhi;
      }
    }
    for (var a = el.parentElement; a; a = a.parentElement) {
      if (a === documentWrapper.documentElement || a === documentWrapper.body && !sw.quirks()) continue;
      var b = hostBox(a[ID]);
      if (!b.precise) continue;
      var maxX = (b.sw || 0) - (b.cw || 0), maxY = (b.sh || 0) - (b.ch || 0);
      if (maxX < 1 && maxY < 1) continue;
      var er = el.getBoundingClientRect(), ar = a.getBoundingClientRect();
      var dy = maxY >= 1 ? delta(er.top, er.bottom, ar.top, ar.top + b.ch, block) : 0;
      var dx = maxX >= 1 ? delta(er.left, er.right, ar.left, ar.left + b.cw, inline) : 0;
      if (dx || dy) scrollSetPos(a, (b.sl || 0) + dx, (b.st || 0) + dy, behavior);
    }
    var r = el.getBoundingClientRect(), vp = viewport();
    var vdy = delta(r.top, r.bottom, 0, vp.height, block), vdx = 0;
    if (vdy || vdx) windowScroll(vdx, vdy, true, behavior);
  }
  // the element a URL fragment names (id, then a[name]); an empty or #top fragment is the top of the page
  function scrollToFragment() {
    var h = ''; try { h = String(global.location.hash || ''); } catch (e) { return; }
    if (h.length < 2) { if (h === '' || h === '#') return; }
    var id = h.slice(1); try { id = decodeURIComponent(id); } catch (e) { /* keep as is */ }
    var el = documentWrapper.getElementById(id);
    if (!el) { var named = documentWrapper.getElementsByName ? documentWrapper.getElementsByName(id) : []; for (var i = 0; i < named.length; i++) if (named[i].localName === 'a') { el = named[i]; break; } }
    if (el) scrollIntoViewImpl(el, { block: 'start', inline: 'nearest' });
    else if (id.toLowerCase() === 'top') windowScroll(0, 0, false, 'auto');
  }
  global.__swScrollToFragment = scrollToFragment;
  function domRect(b) { var p = scrollPos(); return new DOMRect(b.x - p[0], b.y - p[1], b.width, b.height); }
  function ceAttributeChanged(el, name, oldV, newV) {
    var d = ceDefinitions.get(el.localName);
    if (!d || !d.observed || d.observed.indexOf(name) < 0 || typeof el.attributeChangedCallback !== 'function') return;
    try { el.attributeChangedCallback(name, oldV, newV, null); } catch (e) { reportError(e); }
  }

  // --- HTMLElement and subclasses ------------------------------------------
  // HTMLElement's constructor is only callable while upgrading a custom element
  // (or when a defined custom element class is `new`ed directly).
  var HTMLElement = makeClass('HTMLElement', Element, function HTMLElement() {
    if (ceUpgrading) { var w = ceUpgrading; ceUpgrading = null; if (new.target && new.target.prototype !== Object.getPrototypeOf(w)) Object.setPrototypeOf(w, new.target.prototype); return w; }
    var C = new.target;
    var name = null;
    ceDefinitions.forEach(function (d, n) { if (d.ctor === C) name = n; });
    if (!name) illegal();
    var id = sw.createElement(name);
    var w2 = makeWrapper(id, C);
    return w2;
  });
  var activeElement = null;
  getters(HTMLElement.prototype, {
    style: { get: function () { var s = slots(this); return s.style || (s.style = styleDecl(this)); }, set: function (v) { this.style.cssText = toStr(v); } },
    dataset: { get: function () { var s = slots(this); return s.dataset || (s.dataset = dataset(this)); } },
    offsetWidth: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.ow : b.width); } },
    offsetHeight: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.oh : b.height); } },
    offsetTop: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.oy : b.y); } },
    offsetLeft: { get: function () { var b = hostBox(this[ID]); return Math.round(b.precise ? b.ox : b.x); } },
    offsetParent: { get: function () { return this.isConnected && this.localName !== 'body' && this.localName !== 'html' ? (this.parentElement && this.parentElement.closest('body') ? documentWrapper.body : null) : null; } },
    isContentEditable: { get: function () { var v = this.getAttribute('contenteditable'); return v === '' || v === 'true' || v === 'plaintext-only'; } },
    nonce: { get: function () { return this.getAttribute('nonce') || ''; }, set: function (v) { this.setAttribute('nonce', toStr(v)); } }
  });
  methods(HTMLElement.prototype, {
    focus: function () { focusElement(this); },
    blur: function () { if (activeElement === this) blurActive(null); },
    click: function () { clickElement(this); },
    showPopover: function () { showPopoverImpl(this); },
    hidePopover: function () { hidePopoverImpl(this, true, true); },
    togglePopover: function (force) {
      var open = popoverOpen(this);
      if (force === undefined ? open : !force) hidePopoverImpl(this, true, true); else showPopoverImpl(this);
      return popoverOpen(this);
    }
  });
  getters(HTMLElement.prototype, {
    popover: {
      get: function () { var v = this.getAttribute('popover'); if (v === null) return null; v = v.toLowerCase(); return v === '' || v === 'auto' ? 'auto' : 'manual'; },
      set: function (v) { if (v === null || v === undefined) this.removeAttribute('popover'); else this.setAttribute('popover', toStr(v)); }
    }
  });
  // ---- Popover API. The open state lives in the host attribute data-sw-popover-open (the way focus and fullscreen are
  // carried to the style engine), which `:popover-open` and the UA style sheet's display:none for closed popovers read.
  var popoverStack = [], popoverDismissInstalled = false;
  function popoverType(el) { var v = el.getAttribute('popover'); if (v === null) return null; v = v.toLowerCase(); return v === '' || v === 'auto' ? 'auto' : 'manual'; }
  function popoverOpen(el) { var s = el[SLOT]; return !!(s && s.popoverOpen); }
  var popoverEverOpened = false; // innerHTML's fast path is only exact while no popover has ever been opened
  function setPopoverOpen(el, on) {
    if (on) popoverEverOpened = true;
    slots(el).popoverOpen = on;
    try { if (on) sw.setAttr(el[ID], 'data-sw-popover-open', ''); else sw.removeAttr(el[ID], 'data-sw-popover-open'); } catch (e) { /* ignore */ }
    hostMutation('attr', el[ID]);
  }
  // The toggle event is queued as a task; opening and closing again before it runs coalesces into one task that keeps the
  // first old state, and nothing fires when the popover ended up where it started.
  function popoverToggleEvents(el, oldState) {
    var s = slots(el);
    if (s.toggleTask) { oldState = s.toggleTask.old; clearTimer(s.toggleTask.id); }
    var task = s.toggleTask = { old: oldState };
    task.id = setTimeoutImpl(function () {
      s.toggleTask = null;
      var now = popoverOpen(el) ? 'open' : 'closed';
      if (now === task.old) return;
      try { el.dispatchEvent(new global.ToggleEvent('toggle', { oldState: task.old, newState: now })); } catch (e) { reportError(e); }
    }, [], 0, false);
  }
  function requirePopover(el, what) {
    if (!popoverType(el)) throw DOMException("Failed to execute '" + what + "' on 'HTMLElement': The element does not have a 'popover' attribute defined.", 'NotSupportedError');
  }
  function showPopoverImpl(el) {
    requirePopover(el, 'showPopover');
    if (popoverOpen(el)) return;
    if (!el.isConnected) throw DOMException("Failed to execute 'showPopover' on 'HTMLElement': Invalid on disconnected popover elements.", 'InvalidStateError');
    var type = popoverType(el);
    var before = new global.BeforeToggleEvent('beforetoggle', { cancelable: true, oldState: 'closed', newState: 'open' });
    el.dispatchEvent(before);
    if (before.defaultPrevented || popoverOpen(el) || !el.isConnected || popoverType(el) !== type) return;
    if (type === 'auto') {
      // opening an auto popover closes the other auto popovers that do not contain it
      popoverStack.slice().forEach(function (o) { if (o !== el && !o.contains(el) && popoverOpen(o)) hidePopoverImpl(o, true, false); });
      popoverStack.push(el);
      installPopoverDismiss();
    }
    setPopoverOpen(el, true);
    popoverToggleEvents(el, 'closed');
  }
  function hidePopoverImpl(el, events, check) {
    if (check) requirePopover(el, 'hidePopover');
    if (!popoverOpen(el)) return;
    var i = popoverStack.indexOf(el);
    if (i >= 0) popoverStack.splice(i).slice(1).forEach(function (o) { if (popoverOpen(o)) hidePopoverImpl(o, events, false); });
    if (events) el.dispatchEvent(new global.BeforeToggleEvent('beforetoggle', { oldState: 'open', newState: 'closed' }));
    if (!popoverOpen(el)) return;
    setPopoverOpen(el, false);
    if (events) popoverToggleEvents(el, 'open');
  }
  function installPopoverDismiss() {
    if (popoverDismissInstalled) return;
    popoverDismissInstalled = true;
    var downTarget = null;
    var topAuto = function () { return popoverStack.length ? popoverStack[popoverStack.length - 1] : null; };
    documentWrapper.addEventListener('keydown', function (e) { if (e.key === 'Escape' && !e.defaultPrevented && topAuto()) hidePopoverImpl(topAuto(), true, false); }, false);
    documentWrapper.addEventListener('pointerdown', function (e) { downTarget = e.target; }, true);
    documentWrapper.addEventListener('mousedown', function (e) { downTarget = e.target; }, true);
    documentWrapper.addEventListener('click', function (e) {
      var t = e.target, d = downTarget; downTarget = null;
      // light dismiss: a press and release outside every open auto popover (and not on a button that invokes one) closes them
      var invokes = function (n) { var b = n && n.closest ? n.closest('[popovertarget]') : null; return !!b; };
      popoverStack.slice().reverse().forEach(function (p) { if (popoverOpen(p) && !p.contains(t) && !(d && p.contains(d)) && !invokes(t)) hidePopoverImpl(p, true, false); });
    }, false);
  }
  function popoverInvoker(el) {
    var id = el.getAttribute('popovertarget'); if (id === null) return false;
    var t = el.getRootNode && el.getRootNode().getElementById ? el.getRootNode().getElementById(id) : documentWrapper.getElementById(id);
    if (!t || !popoverType(t)) return true;
    var action = (el.getAttribute('popovertargetaction') || 'toggle').toLowerCase();
    if (action !== 'show' && action !== 'hide') action = 'toggle';
    var open = popoverOpen(t);
    if (open && action !== 'show') hidePopoverImpl(t, true, false); else if (!open && action !== 'hide') { try { showPopoverImpl(t); } catch (e) { /* not connected */ } }
    return true;
  }
  function focusable(el) { return el && el.nodeType === 1; }
  function blurActive(next) {
    var prev = activeElement;
    activeElement = null;
    if (host('setFocus')) { try { sw.setFocus(0); } catch (e) { /* ignore */ } }
    if (prev && prev.isConnected) { fire(prev, 'blur', { bubbles: false, cancelable: false, relatedTarget: next }); fire(prev, 'focusout', { bubbles: true, cancelable: false, relatedTarget: next }); }
  }
  function focusElement(el) {
    if (!focusable(el) || activeElement === el || !el.isConnected) return;
    var prev = activeElement;
    blurActive(el);
    activeElement = el;
    if (host('setFocus')) { try { sw.setFocus(el[ID]); } catch (e) { /* ignore */ } }
    fire(el, 'focus', { bubbles: false, cancelable: false, relatedTarget: prev });
    fire(el, 'focusin', { bubbles: true, cancelable: false, relatedTarget: prev });
  }
  // Checkbox/radio state flips BEFORE click listeners run and is reverted when
  // the event is cancelled (matches browsers; React reads .checked in onClick).
  function preClick(el) {
    if (el.localName !== 'input') return null;
    var type = (el.getAttribute('type') || 'text').toLowerCase();
    if (type === 'checkbox') { var was = sw.getChecked(el[ID]); sw.setChecked(el[ID], !was); return function () { sw.setChecked(el[ID], was); }; }
    if (type === 'radio') { var wasR = sw.getChecked(el[ID]); var others = []; radioGroup(el).forEach(function (r) { if (r !== el && sw.getChecked(r[ID])) { others.push(r); sw.setChecked(r[ID], false); } }); sw.setChecked(el[ID], true); return function () { sw.setChecked(el[ID], wasR); others.forEach(function (r) { sw.setChecked(r[ID], true); }); }; }
    return null;
  }
  function radioGroup(el) {
    var name = el.getAttribute('name'); if (!name) return [el];
    var root = el.form || documentWrapper;
    return queryAll(root, 'input[type="radio"][name="' + cssStr(name) + '"]');
  }
  function defaultAction(el) {
    if (!el || el.nodeType !== 1) return;
    var tag = el.localName;
    if ((tag === 'button' || tag === 'input') && el.hasAttribute('popovertarget') && !el.hasAttribute('disabled')) {
      var itype = (el.getAttribute('type') || (tag === 'button' ? 'submit' : 'text')).toLowerCase();
      if (tag === 'button' || itype === 'button' || itype === 'submit' || itype === 'reset' || itype === 'image') popoverInvoker(el);
    }
    if (tag === 'input') {
      var type = (el.getAttribute('type') || 'text').toLowerCase();
      if (type === 'checkbox' || type === 'radio') { fire(el, 'input', { bubbles: true, cancelable: false }); fire(el, 'change', { bubbles: true, cancelable: false }); return; }
      if (type === 'submit' || type === 'image') { submitFrom(el); return; }
      if (type === 'reset') { var f = el.form; if (f) f.reset(); return; }
      return;
    }
    if (tag === 'button') { var bt = (el.getAttribute('type') || 'submit').toLowerCase(); if (bt === 'submit') submitFrom(el); else if (bt === 'reset' && el.form) el.form.reset(); return; }
    if (tag === 'label') { var c = el.control; if (c) clickElement(c); return; }
    if (tag === 'summary') { var d = el.parentElement; if (d && d.localName === 'details') { d.open = !d.open; fire(d, 'toggle', { bubbles: false, cancelable: false }); } return; }
    var a = el.closest('a[href]') || el.closest('area[href]');
    if (a) {
      var href = a.getAttribute('href');
      if (href && !/^\s*javascript:/i.test(href)) {
        if (a.hasAttribute('download')) { startDownload(a.href, a.getAttribute('download') || ''); return; }
        if (/^#/.test(href.trim())) { global.location.hash = href.trim(); } else navigateLink(a);
      }
    }
  }
  // a link with a target other than _self opens in a frame, another window or a new one (host: windows.go)
  function navigateLink(a) {
    var tg = a.getAttribute('target');
    if (tg && !/^\s*_self\s*$/i.test(tg) && typeof sw.frName === 'function') { sw.frName('nav', a.href, tg, a.getAttribute('rel') || ''); return; }
    sw.navigate(a.href);
  }
  // <a download>: blob: URLs are read here (their bytes live in this VM);
  // everything else is fetched by Go into the downloads directory.
  function startDownload(url, name) {
    if (/^blob:/.test(url)) {
      var blob = blobURLs.get(url);
      if (!blob) { reportError(new Error('download: unknown blob URL ' + url)); return; }
      var bytes = blob._bytes || new Uint8Array(0), bin = '';
      for (var i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
      sw.saveDownload(name || 'download', global.btoa(bin), blob.type || '', url);
      return;
    }
    sw.download(url, name);
  }
  function submitFrom(submitter) {
    var form = submitter.form || (submitter.hasAttribute('form') ? documentWrapper.getElementById(submitter.getAttribute('form')) : null);
    if (!form) return;
    submitForm(form, submitter);
  }
  function submitForm(form, submitter) {
    if (!fire(form, 'submit', { bubbles: true, cancelable: true, submitter: submitter || null })) return;
    sw.submitForm(form[ID]);
  }
  function clickElement(el) {
    if (el.localName && el.hasAttribute('disabled') && /^(button|input|select|textarea|fieldset|option)$/.test(el.localName)) return;
    var revert = preClick(el);
    var ok = fire(el, 'click', { bubbles: true, cancelable: true, composed: true, button: 0, buttons: 0, detail: 1 });
    if (!ok) { if (revert) revert(); return false; }
    defaultAction(el);
    return true;
  }

  // Subclass table: class name -> tags. Generated classes; specialised below.
  var HTML_CLASSES = {
    HTMLHtmlElement: ['html'], HTMLHeadElement: ['head'], HTMLBodyElement: ['body'], HTMLTitleElement: ['title'], HTMLMetaElement: ['meta'], HTMLLinkElement: ['link'], HTMLBaseElement: ['base'],
    HTMLStyleElement: ['style'], HTMLScriptElement: ['script'], HTMLTemplateElement: ['template'], HTMLSlotElement: ['slot'],
    HTMLDivElement: ['div'], HTMLSpanElement: ['span'], HTMLParagraphElement: ['p'], HTMLHeadingElement: ['h1', 'h2', 'h3', 'h4', 'h5', 'h6'], HTMLPreElement: ['pre'], HTMLBRElement: ['br'], HTMLHRElement: ['hr'], HTMLQuoteElement: ['blockquote', 'q'],
    HTMLAnchorElement: ['a'], HTMLImageElement: ['img'], HTMLButtonElement: ['button'], HTMLInputElement: ['input'], HTMLSelectElement: ['select'], HTMLOptionElement: ['option'], HTMLOptGroupElement: ['optgroup'], HTMLTextAreaElement: ['textarea'], HTMLFormElement: ['form'], HTMLLabelElement: ['label'], HTMLFieldSetElement: ['fieldset'], HTMLLegendElement: ['legend'], HTMLDataListElement: ['datalist'], HTMLOutputElement: ['output'], HTMLProgressElement: ['progress'], HTMLMeterElement: ['meter'],
    HTMLUListElement: ['ul'], HTMLOListElement: ['ol'], HTMLLIElement: ['li'], HTMLDListElement: ['dl'], HTMLMenuElement: ['menu'],
    HTMLTableElement: ['table'], HTMLTableSectionElement: ['thead', 'tbody', 'tfoot'], HTMLTableRowElement: ['tr'], HTMLTableCellElement: ['td', 'th'], HTMLTableCaptionElement: ['caption'], HTMLTableColElement: ['col', 'colgroup'],
    HTMLCanvasElement: ['canvas'], HTMLMediaElement: [], HTMLVideoElement: ['video'], HTMLAudioElement: ['audio'], HTMLSourceElement: ['source'], HTMLTrackElement: ['track'], HTMLPictureElement: ['picture'],
    HTMLIFrameElement: ['iframe'], HTMLEmbedElement: ['embed'], HTMLObjectElement: ['object'], HTMLAreaElement: ['area'], HTMLMapElement: ['map'],
    HTMLDetailsElement: ['details'], HTMLDialogElement: ['dialog'], HTMLTimeElement: ['time'], HTMLDataElement: ['data'], HTMLModElement: ['ins', 'del'], HTMLUnknownElement: []
  };
  Object.keys(HTML_CLASSES).forEach(function (name) {
    var sup = name === 'HTMLVideoElement' || name === 'HTMLAudioElement' ? global.HTMLMediaElement : HTMLElement;
    var C = makeClass(name, sup, function () { return Reflect.construct(HTMLElement, arguments, new.target || C); });
    HTML_CLASSES[name].forEach(function (tag) { TAG_CLASS[tag] = C; });
  });
  // form, labels and the constraint validation API belong to the form-associated elements only, not to every HTMLElement:
  // a script that probes `'labels' in el` or `'form' in el` to tell native properties from its own must see the real shape.
  (function () {
    var formG = { get: function () { return this.closest('form'); } };
    var labelsG = { get: function () { var id = this.id; return nodeListOf(id ? queryAll(documentWrapper, 'label[for="' + cssStr(id) + '"]') : []); } };
    var validation = { checkValidity: function () { return true; }, reportValidity: function () { return true; }, setCustomValidity: function () { } };
    var constraint = {
    validity: { get: function () { return { valid: true, valueMissing: false, typeMismatch: false, patternMismatch: false, tooLong: false, tooShort: false, rangeUnderflow: false, rangeOverflow: false, stepMismatch: false, badInput: false, customError: false }; } },
      validationMessage: { get: function () { return ''; } },
      willValidate: { get: function () { return true; } }
    };
    ['HTMLInputElement', 'HTMLSelectElement', 'HTMLTextAreaElement', 'HTMLButtonElement', 'HTMLFieldSetElement', 'HTMLOutputElement', 'HTMLObjectElement'].forEach(function (n) {
      if (!global[n]) return;
      var P = global[n].prototype;
      getters(P, { form: formG });
      getters(P, constraint);
      methods(P, validation);
    });
    ['HTMLInputElement', 'HTMLSelectElement', 'HTMLTextAreaElement', 'HTMLButtonElement', 'HTMLOutputElement', 'HTMLMeterElement', 'HTMLProgressElement'].forEach(function (n) {
      if (global[n]) getters(global[n].prototype, { labels: labelsG });
    });
    ['HTMLLabelElement', 'HTMLLegendElement'].forEach(function (n) { if (global[n]) getters(global[n].prototype, { form: formG }); });
  })();
  var SVGElement = makeClass('SVGElement', Element);
  getters(SVGElement.prototype, { style: Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'style'), dataset: Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'dataset'), ownerSVGElement: { get: function () { var p = this.parentNode; return p && p.closest ? p.closest('svg') : null; } }, viewportElement: { get: function () { return this.ownerSVGElement; } } });
  methods(SVGElement.prototype, { focus: HTMLElement.prototype.focus, blur: HTMLElement.prototype.blur, getBBox: function () { return this.getBoundingClientRect(); }, getScreenCTM: function () { return null; }, getCTM: function () { return null; }, createSVGPoint: function () { return { x: 0, y: 0, matrixTransform: function () { return this; } }; } });
  global.SVGSVGElement = makeClass('SVGSVGElement', SVGElement); TAG_CLASS.svg = global.SVGSVGElement;
  global.SVGGraphicsElement = SVGElement;
  // SVG element interfaces by tag (pages test nodes with instanceof SVGPathElement and the like)
  var SVG_CLASS = {};
  var SVG_CLASSES = { SVGPathElement: 'path', SVGCircleElement: 'circle', SVGRectElement: 'rect', SVGEllipseElement: 'ellipse', SVGLineElement: 'line',
    SVGPolylineElement: 'polyline', SVGPolygonElement: 'polygon', SVGGElement: 'g', SVGDefsElement: 'defs', SVGUseElement: 'use',
    SVGSymbolElement: 'symbol', SVGTextElement: 'text', SVGTSpanElement: 'tspan', SVGImageElement: 'image', SVGClipPathElement: 'clippath',
    SVGMaskElement: 'mask', SVGPatternElement: 'pattern', SVGLinearGradientElement: 'lineargradient', SVGRadialGradientElement: 'radialgradient',
    SVGStopElement: 'stop', SVGMarkerElement: 'marker', SVGForeignObjectElement: 'foreignobject', SVGFilterElement: 'filter' };
  Object.keys(SVG_CLASSES).forEach(function (name) {
    var tag = SVG_CLASSES[name], C = makeClass(name, SVGElement);
    global[name] = C; SVG_CLASS[tag] = C; if (!TAG_CLASS[tag]) TAG_CLASS[tag] = C;
  });

  // ---- form controls: live value/checked accessors live on the SPECIFIC prototypes
  function liveValue(proto) {
    def(proto, 'value', { get: function () { var v = sw.getValue(this[ID]); return v === null || v === undefined ? '' : String(v); }, set: function (v) { v = v === null || v === undefined ? '' : String(v); sw.setValue(this[ID], v); slots(this).dirtyValue = true; hostMutation('value', this[ID]); }, enumerable: true });
  }
  var InputP = global.HTMLInputElement.prototype;
  liveValue(InputP); liveValue(global.HTMLTextAreaElement.prototype); liveValue(global.HTMLSelectElement.prototype);
  (function () { // checkbox/radio default to "on" when no value attribute is present
    var base = Object.getOwnPropertyDescriptor(InputP, 'value');
    def(InputP, 'value', { get: function () { var v = base.get.call(this); if (v === '' && !sw.hasAttr(this[ID], 'value')) { var t = (sw.getAttr(this[ID], 'type') || '').toLowerCase(); if (t === 'checkbox' || t === 'radio') return 'on'; } return v; }, set: base.set, enumerable: true });
  })();
  getters(InputP, {
    checked: { get: function () { return !!sw.getChecked(this[ID]); }, set: function (v) { sw.setChecked(this[ID], !!v); slots(this).dirtyChecked = true; hostMutation('checked', this[ID]); } },
    defaultChecked: { get: function () { return this.hasAttribute('checked'); }, set: function (v) { this.toggleAttribute('checked', !!v); } },
    defaultValue: { get: function () { return this.getAttribute('value') || ''; }, set: function (v) { this.setAttribute('value', toStr(v)); } },
    type: { get: function () { var t = (this.getAttribute('type') || 'text').toLowerCase(); return /^(text|password|checkbox|radio|submit|reset|button|file|hidden|image|email|number|tel|url|search|date|time|datetime-local|month|week|color|range)$/.test(t) ? t : 'text'; }, set: function (v) { this.setAttribute('type', toStr(v)); } },
    valueAsNumber: { get: function () { var n = parseFloat(this.value); return isNaN(n) ? NaN : n; }, set: function (v) { this.value = isNaN(v) ? '' : String(v); } },
    valueAsDate: { get: function () { var d = new Date(this.value); return isNaN(d.getTime()) ? null : d; }, set: function (v) { this.value = v ? new Date(v).toISOString().slice(0, 10) : ''; } },
    files: { get: function () { var sl = slots(this); return sl.files || (this.type === 'file' ? (sl.files = makeFileList([])) : null); }, set: function (v) { if (v instanceof FileList || v === null) slots(this).files = v; else if (v && typeof v.length === 'number') slots(this).files = makeFileList(Array.prototype.slice.call(v)); } },
    selectionStart: { get: function () { var s = slots(this); return s.selStart === undefined ? this.value.length : Math.min(s.selStart, this.value.length); }, set: function (v) { slots(this).selStart = v | 0; } },
    selectionEnd: { get: function () { var s = slots(this); return s.selEnd === undefined ? this.value.length : Math.min(s.selEnd, this.value.length); }, set: function (v) { slots(this).selEnd = v | 0; } },
    selectionDirection: { get: function () { return 'none'; }, set: function () { } },
    list: { get: function () { var id = this.getAttribute('list'); return id ? documentWrapper.getElementById(id) : null; } },
    indeterminate: { get: function () { return !!slots(this).indeterminate; }, set: function (v) { slots(this).indeterminate = !!v; } }
  });
  methods(InputP, {
    select: function () { slots(this).selStart = 0; slots(this).selEnd = this.value.length; fire(this, 'select', { bubbles: true, cancelable: false }); },
    setSelectionRange: function (s, e) { slots(this).selStart = s | 0; slots(this).selEnd = e | 0; },
    setRangeText: function (text, s, e) { var v = this.value; if (s === undefined) { s = this.selectionStart; e = this.selectionEnd; } this.value = v.slice(0, s) + text + v.slice(e); },
    stepUp: function (n) { this.valueAsNumber = (this.valueAsNumber || 0) + (n === undefined ? 1 : n) * (parseFloat(this.getAttribute('step')) || 1); },
    stepDown: function (n) { this.stepUp(-(n === undefined ? 1 : n)); },
    showPicker: function () { }
  });
  var TextAreaP = global.HTMLTextAreaElement.prototype;
  getters(TextAreaP, {
    defaultValue: { get: function () { return this.textContent; }, set: function (v) { this.textContent = toStr(v); } },
    type: { get: function () { return 'textarea'; } },
    textLength: { get: function () { return this.value.length; } },
    selectionStart: Object.getOwnPropertyDescriptor(InputP, 'selectionStart'), selectionEnd: Object.getOwnPropertyDescriptor(InputP, 'selectionEnd')
  });
  methods(TextAreaP, { select: InputP.select, setSelectionRange: InputP.setSelectionRange, setRangeText: InputP.setRangeText });
  var SelectP = global.HTMLSelectElement.prototype;
  getters(SelectP, {
    selectedIndex: { get: function () { var i = sw.selectedIndex(this[ID]); return typeof i === 'number' ? i : -1; }, set: function (i) { sw.setSelectedIndex(this[ID], i | 0); hostMutation('value', this[ID]); } },
    options: { get: function () { var self = this; var s = slots(this); return s.options || (s.options = liveList(HTMLCollection, genCached(function () { return wrapAll(sw.elementsByTag(self[ID], 'option')); }))); } },
    selectedOptions: { get: function () { return collectionOf(itemsOf(this.options).filter(function (o) { return o.selected; })); } },
    length: { get: function () { return this.options.length; }, set: function () { } },
    type: { get: function () { return this.hasAttribute('multiple') ? 'select-multiple' : 'select-one'; } }
  });
  methods(SelectP, {
    item: function (i) { return this.options[i] || null; },
    namedItem: function (n) { return this.options.namedItem(n); },
    add: function (opt, before) { if (typeof before === 'number') before = this.options[before] || null; if (before) before.parentNode.insertBefore(opt, before); else this.appendChild(opt); },
    remove: function (i) { if (arguments.length === 0) return ChildNodeMethods.remove.call(this); var o = this.options[i]; if (o) o.remove(); }
  });
  var OptionP = global.HTMLOptionElement.prototype;
  getters(OptionP, {
    value: { get: function () { var v = this.getAttribute('value'); return v === null ? this.text : v; }, set: function (v) { this.setAttribute('value', toStr(v)); } },
    text: { get: function () { return this.textContent.replace(/\s+/g, ' ').trim(); }, set: function (v) { this.textContent = toStr(v); } },
    label: { get: function () { var l = this.getAttribute('label'); return l === null ? this.text : l; }, set: function (v) { this.setAttribute('label', toStr(v)); } },
    index: { get: function () { var sel = this.closest('select'); return sel ? itemsOf(sel.options).indexOf(this) : 0; } },
    defaultSelected: { get: function () { return this.hasAttribute('selected'); }, set: function (v) { this.toggleAttribute('selected', !!v); } },
    selected: {
      get: function () { var sel = this.closest('select'); if (!sel) return !!slots(this).selected || this.hasAttribute('selected'); if (sel.hasAttribute('multiple')) return slots(this).selected !== undefined ? !!slots(this).selected : this.hasAttribute('selected'); return sel.selectedIndex === this.index; },
      set: function (v) { v = !!v; slots(this).selected = v; var sel = this.closest('select'); if (!sel) return; if (v) sel.selectedIndex = this.index; else if (sel.selectedIndex === this.index) sel.selectedIndex = -1; }
    },
    form: { get: function () { return this.closest('form'); } }
  });
  getters(global.HTMLButtonElement.prototype, {
    type: { get: function () { var t = (this.getAttribute('type') || 'submit').toLowerCase(); return /^(submit|reset|button)$/.test(t) ? t : 'submit'; }, set: function (v) { this.setAttribute('type', toStr(v)); } }
  });
  var FormP = global.HTMLFormElement.prototype;
  var FORM_CONTROLS = 'input,select,textarea,button,fieldset,output,object';
  getters(FormP, {
    elements: { get: function () { var self = this; var s = slots(this); return s.elements || (s.elements = liveList(HTMLCollection, function () { return queryAll(self, FORM_CONTROLS).filter(function (c) { return !(c.localName === 'input' && c.type === 'image'); }); })); } },
    length: { get: function () { return this.elements.length; } }
  });
  methods(FormP, {
    submit: function () { sw.submitForm(this[ID]); },
    requestSubmit: function (submitter) { submitForm(this, submitter || null); },
    reset: function () {
      if (!fire(this, 'reset', { bubbles: true, cancelable: true })) return;
      itemsOf(this.elements).forEach(function (c) {
        if (c.localName === 'input') { if (c.type === 'checkbox' || c.type === 'radio') c.checked = c.defaultChecked; else if (c.type !== 'file') c.value = c.defaultValue; }
        else if (c.localName === 'textarea') c.value = c.defaultValue;
        else if (c.localName === 'select') { var opts = itemsOf(c.options); var idx = -1; opts.forEach(function (o, i) { if (o.hasAttribute('selected')) idx = i; }); c.selectedIndex = idx < 0 && opts.length && !c.hasAttribute('multiple') ? 0 : idx; }
      });
    },
    checkValidity: function () { return true; }, reportValidity: function () { return true; }
  });
  getters(global.HTMLLabelElement.prototype, {
    control: { get: function () { var f = this.getAttribute('for'); if (f) return documentWrapper.getElementById(f); return queryOne(this, 'input,select,textarea,button,meter,output,progress'); } }
  });
  getters(global.HTMLFieldSetElement.prototype, { elements: Object.getOwnPropertyDescriptor(FormP, 'elements'), type: { get: function () { return 'fieldset'; } } });

  // ---- anchors / areas: URL decomposition
  var URL_PARTS = ['protocol', 'host', 'hostname', 'port', 'pathname', 'search', 'hash', 'origin', 'username', 'password'];
  function defineHyperlink(proto) {
    def(proto, 'href', { get: function () { var h = this.getAttribute('href'); if (h === null) return ''; try { return new global.URL(h, currentURL()).href; } catch (e) { return h; } }, set: function (v) { this.setAttribute('href', toStr(v)); }, enumerable: true });
    URL_PARTS.forEach(function (part) {
      def(proto, part, { get: function () { try { return new global.URL(this.href)[part]; } catch (e) { return ''; } }, set: function (v) { try { var u = new global.URL(this.href); u[part] = v; this.href = u.href; } catch (e) { /* ignore */ } }, enumerable: true });
    });
    method(proto, 'toString', function () { return this.href; });
  }
  defineHyperlink(global.HTMLAnchorElement.prototype); defineHyperlink(global.HTMLAreaElement.prototype);
  getters(global.HTMLAnchorElement.prototype, {
    relList: { get: function () { var s = slots(this); return s.relList || (s.relList = tokenList(this, 'rel')); } },
    text: { get: function () { return this.textContent; }, set: function (v) { this.textContent = toStr(v); } }
  });
  getters(global.HTMLLinkElement.prototype, { relList: Object.getOwnPropertyDescriptor(global.HTMLAnchorElement.prototype, 'relList'), sheet: { get: function () { return null; } }, href: { get: function () { var h = this.getAttribute('href'); if (h === null) return ''; try { return new global.URL(h, currentURL()).href; } catch (e) { return h; } }, set: function (v) { this.setAttribute('href', toStr(v)); } } });
  getters(global.HTMLBaseElement.prototype, { href: Object.getOwnPropertyDescriptor(global.HTMLLinkElement.prototype, 'href') });

  // ---- images / media / canvas / iframe / script / template
  var ImgP = global.HTMLImageElement.prototype;
  getters(ImgP, {
    src: { get: function () { var s = this.getAttribute('src'); if (s === null) return ''; try { return new global.URL(s, currentURL()).href; } catch (e) { return s; } }, set: function (v) { this.setAttribute('src', toStr(v)); scheduleLoadEvent(this); } },
    currentSrc: { get: function () { return this.src; } },
    naturalWidth: { get: function () { return hostBox(this[ID]).width | 0; } }, naturalHeight: { get: function () { return hostBox(this[ID]).height | 0; } },
    complete: { get: function () { return true; } },
    x: { get: function () { return hostBox(this[ID]).x | 0; } }, y: { get: function () { return hostBox(this[ID]).y | 0; } }
  });
  methods(ImgP, { decode: function () { return Promise.resolve(); } });
  ['HTMLScriptElement', 'HTMLIFrameElement', 'HTMLSourceElement', 'HTMLEmbedElement', 'HTMLTrackElement', 'HTMLMediaElement'].forEach(function (n) { getters(global[n].prototype, { src: Object.getOwnPropertyDescriptor(ImgP, 'src') }); });
  getters(global.HTMLScriptElement.prototype, { text: { get: function () { return this.textContent; }, set: function (v) { this.textContent = toStr(v); } } });
  global.HTMLScriptElement.supports = function (t) { return t === 'classic' || t === 'module'; };
  getters(global.HTMLIFrameElement.prototype, { contentWindow: { get: function () { return null; } }, contentDocument: { get: function () { return null; } } });
  getters(global.HTMLObjectElement.prototype, { contentWindow: { get: function () { return null; } }, contentDocument: { get: function () { return null; } } });
  methods(global.HTMLCanvasElement.prototype, { getContext: function () { return null; }, toDataURL: function () { return 'data:,'; }, toBlob: function (cb) { setTimeoutImpl(function () { cb(null); }, [], 0, false); }, captureStream: function () { return null; } });
  getters(global.HTMLCanvasElement.prototype, { width: { get: function () { var w = parseInt(this.getAttribute('width'), 10); return isNaN(w) ? 300 : w; }, set: function (v) { this.setAttribute('width', String(v | 0)); } }, height: { get: function () { var h = parseInt(this.getAttribute('height'), 10); return isNaN(h) ? 150 : h; }, set: function (v) { this.setAttribute('height', String(v | 0)); } } });
  var MediaP = global.HTMLMediaElement.prototype;
  getters(MediaP, {
    paused: { get: function () { return slots(this).playing !== true; } }, ended: { get: function () { return false; } }, duration: { get: function () { return NaN; } }, readyState: { get: function () { return 0; } }, networkState: { get: function () { return 0; } }, seeking: { get: function () { return false; } }, buffered: { get: function () { return { length: 0, start: function () { return 0; }, end: function () { return 0; } }; } }, error: { get: function () { return null; } }, canPlayType: { get: function () { return function () { return ''; }; } },
    currentTime: { get: function () { return slots(this).currentTime || 0; }, set: function (v) { slots(this).currentTime = +v || 0; } },
    volume: { get: function () { return slots(this).volume === undefined ? 1 : slots(this).volume; }, set: function (v) { slots(this).volume = +v; } },
    playbackRate: { get: function () { return slots(this).rate || 1; }, set: function (v) { slots(this).rate = +v; } },
    currentSrc: { get: function () { return this.src; } }
  });
  methods(MediaP, {
    play: function () { slots(this).playing = true; fire(this, 'play', { bubbles: false, cancelable: false }); return Promise.resolve(); },
    pause: function () { slots(this).playing = false; fire(this, 'pause', { bubbles: false, cancelable: false }); },
    load: function () { }, addTextTrack: function () { return { addCue: function () { }, mode: 'hidden' }; }
  });
  getters(global.HTMLTemplateElement.prototype, {
    content: {
      get: function () {
        var s = slots(this);
        if (!s.content) { s.content = documentWrapper.createDocumentFragment(); Object.defineProperty(s.content, INERT, { value: true }); }
        var kids = wrapAll(sw.children(this[ID]));
        var self = this;
        kids.forEach(function (k) { sw.removeChild(self[ID], k[ID]); s.content._kids.push(k); k[FRAG] = s.content; });
        return s.content;
      }
    }
  });
  getters(global.HTMLDetailsElement.prototype, { open: { get: function () { return this.hasAttribute('open'); }, set: function (v) { var was = this.hasAttribute('open'); this.toggleAttribute('open', !!v); if (was !== !!v) { var self = this; setTimeoutImpl(function () { fire(self, 'toggle', { bubbles: false, cancelable: false }); }, [], 0, false); } } } });
  getters(global.HTMLDialogElement.prototype, { open: { get: function () { return this.hasAttribute('open'); }, set: function (v) { this.toggleAttribute('open', !!v); } }, returnValue: { get: function () { return slots(this).returnValue || ''; }, set: function (v) { slots(this).returnValue = toStr(v); } } });
  methods(global.HTMLDialogElement.prototype, { show: function () { this.open = true; }, showModal: function () { this.open = true; }, close: function (rv) { if (rv !== undefined) this.returnValue = rv; this.open = false; fire(this, 'close', { bubbles: false, cancelable: false }); } });
  getters(global.HTMLTableElement.prototype, {
    rows: { get: function () { return collectionOf(queryAll(this, 'tr')); } },
    tBodies: { get: function () { return collectionOf(childElements(this[ID]).filter(function (e) { return e.localName === 'tbody'; })); } },
    tHead: { get: function () { return childElements(this[ID]).filter(function (e) { return e.localName === 'thead'; })[0] || null; } },
    tFoot: { get: function () { return childElements(this[ID]).filter(function (e) { return e.localName === 'tfoot'; })[0] || null; } },
    caption: { get: function () { return childElements(this[ID]).filter(function (e) { return e.localName === 'caption'; })[0] || null; } }
  });
  methods(global.HTMLTableElement.prototype, {
    insertRow: function (i) { var body = this.tBodies[0] || this.appendChild(documentWrapper.createElement('tbody')); return body.insertRow(i); },
    deleteRow: function (i) { var r = this.rows[i < 0 ? this.rows.length + i : i]; if (r) r.remove(); },
    createTBody: function () { return this.appendChild(documentWrapper.createElement('tbody')); },
    createTHead: function () { return this.tHead || this.insertBefore(documentWrapper.createElement('thead'), this.firstChild); },
    createCaption: function () { return this.caption || this.insertBefore(documentWrapper.createElement('caption'), this.firstChild); }
  });
  getters(global.HTMLTableSectionElement.prototype, { rows: { get: function () { return collectionOf(childElements(this[ID]).filter(function (e) { return e.localName === 'tr'; })); } } });
  methods(global.HTMLTableSectionElement.prototype, { insertRow: function (i) { var tr = documentWrapper.createElement('tr'); var rows = this.rows; if (i === undefined || i === -1 || i >= rows.length) this.appendChild(tr); else this.insertBefore(tr, rows[i]); return tr; }, deleteRow: function (i) { var r = this.rows[i]; if (r) r.remove(); } });
  getters(global.HTMLTableRowElement.prototype, { cells: { get: function () { return collectionOf(childElements(this[ID]).filter(function (e) { return e.localName === 'td' || e.localName === 'th'; })); } }, rowIndex: { get: function () { var t = this.closest('table'); return t ? itemsOf(t.rows).indexOf(this) : -1; } }, sectionRowIndex: { get: function () { var p = this.parentNode; return p && p.rows ? itemsOf(p.rows).indexOf(this) : -1; } } });
  methods(global.HTMLTableRowElement.prototype, { insertCell: function (i) { var td = documentWrapper.createElement('td'); var cells = this.cells; if (i === undefined || i === -1 || i >= cells.length) this.appendChild(td); else this.insertBefore(td, cells[i]); return td; }, deleteCell: function (i) { var c = this.cells[i]; if (c) c.remove(); } });
  getters(global.HTMLTableCellElement.prototype, { cellIndex: { get: function () { var p = this.parentNode; return p && p.cells ? itemsOf(p.cells).indexOf(this) : -1; } } });
  getters(global.HTMLSlotElement.prototype, { assignedNodes: { get: function () { return function () { return []; }; } }, assignedElements: { get: function () { return function () { return []; }; } } });
  getters(global.HTMLOutputElement.prototype, { value: { get: function () { return this.textContent; }, set: function (v) { this.textContent = toStr(v); } } });
  getters(global.HTMLProgressElement.prototype, { value: { get: function () { return parseFloat(this.getAttribute('value')) || 0; }, set: function (v) { this.setAttribute('value', String(+v)); } }, position: { get: function () { return this.hasAttribute('value') ? this.value / (this.max || 1) : -1; } } });
  getters(global.HTMLMeterElement.prototype, { value: Object.getOwnPropertyDescriptor(global.HTMLProgressElement.prototype, 'value') });
  getters(global.HTMLTimeElement.prototype, { dateTime: { get: function () { return this.getAttribute('datetime') || ''; }, set: function (v) { this.setAttribute('datetime', toStr(v)); } } });
  getters(global.HTMLLIElement.prototype, { value: { get: function () { return parseInt(this.getAttribute('value'), 10) || 0; }, set: function (v) { this.setAttribute('value', String(v | 0)); } } });
  getters(global.HTMLDataElement.prototype, { value: { get: function () { return this.getAttribute('value') || ''; }, set: function (v) { this.setAttribute('value', toStr(v)); } } });
  getters(global.HTMLStyleElement.prototype, { sheet: { get: function () { var s = slots(this); return s.sheet || (s.sheet = { cssRules: [], rules: [], insertRule: function (r, i) { this.cssRules.splice(i || 0, 0, { cssText: r }); return i || 0; }, deleteRule: function (i) { this.cssRules.splice(i, 1); }, ownerNode: this, disabled: false, href: null, type: 'text/css' }); } } });

  // --- CharacterData: Text, Comment ----------------------------------------
  var CharacterData = makeClass('CharacterData', Node);
  getters(CharacterData.prototype, {
    data: { get: function () { return sw.nodeValue(this[ID]); }, set: function (v) { setCharacterData(this, toStr(v)); } },
    length: { get: function () { return sw.nodeValue(this[ID]).length; } }
  });
  getters(CharacterData.prototype, { nextElementSibling: Object.getOwnPropertyDescriptor(Element.prototype, 'nextElementSibling'), previousElementSibling: Object.getOwnPropertyDescriptor(Element.prototype, 'previousElementSibling') });
  methods(CharacterData.prototype, ChildNodeMethods);
  methods(CharacterData.prototype, {
    substringData: function (o, c) { return this.data.substr(o, c); },
    appendData: function (s) { var d = this.data; s = toStr(s); if (this.nodeType === 3 || this.nodeType === 8) setCharacterData(this, d + s, d.length, 0, s.length); else this.data = d + s; },
    insertData: function (o, s) { var d = this.data; if (o > d.length) throw DOMException('The offset ' + o + ' is greater than the node\'s length (' + d.length + ').', 'IndexSizeError'); s = toStr(s); setCharacterData(this, d.slice(0, o) + s + d.slice(o), o, 0, s.length); },
    deleteData: function (o, c) { var d = this.data; if (o > d.length) throw DOMException('The offset ' + o + ' is greater than the node\'s length (' + d.length + ').', 'IndexSizeError'); c = Math.min(c, d.length - o); setCharacterData(this, d.slice(0, o) + d.slice(o + c), o, c, 0); },
    replaceData: function (o, c, s) { var d = this.data; if (o > d.length) throw DOMException('The offset ' + o + ' is greater than the node\'s length (' + d.length + ').', 'IndexSizeError'); c = Math.min(c, d.length - o); s = toStr(s); setCharacterData(this, d.slice(0, o) + s + d.slice(o + c), o, c, s.length); }
  });
  var Text = makeClass('Text', CharacterData, function Text(data) { var id = sw.createText(arguments.length ? toStr(data) : ''); var w = makeWrapper(id, new.target || Text); return w; });
  getters(Text.prototype, { wholeText: { get: function () { var s = this.data; for (var n = this.previousSibling; n && n.nodeType === 3; n = n.previousSibling) s = n.data + s; for (n = this.nextSibling; n && n.nodeType === 3; n = n.nextSibling) s += n.data; return s; } }, assignedSlot: { get: function () { return null; } } });
  methods(Text.prototype, { splitText: function (offset) { var d = this.data; if (offset > d.length) throw DOMException('The offset is larger than the length.', 'IndexSizeError'); var t = new Text(d.slice(offset)); var p = this.parentNode; if (p) { p.insertBefore(t, this.nextSibling); rn('split', this, offset, t); } setCharacterData(this, d.slice(0, offset), -1); return t; } });
  var Comment = makeClass('Comment', CharacterData, function Comment(data) { var id = sw.createComment(arguments.length ? toStr(data) : ''); return makeWrapper(id, new.target || Comment); });
  var DocumentType = makeClass('DocumentType', Node);
  getters(DocumentType.prototype, { name: { get: function () { return sw.tagName(this[ID]) || 'html'; } }, publicId: { get: function () { return ''; } }, systemId: { get: function () { return ''; } } });
  methods(DocumentType.prototype, ChildNodeMethods);
  global.CDATASection = makeClass('CDATASection', Text);
  global.ProcessingInstruction = makeClass('ProcessingInstruction', CharacterData);

  // --- DocumentFragment: JS-only container; children move into the host tree on insert
  var fragSeq = -1;
  var DocumentFragment = makeClass('DocumentFragment', Node, function DocumentFragment() {
    var f = Object.create(new.target ? new.target.prototype : DocumentFragment.prototype);
    Object.defineProperty(f, ID, { value: fragSeq-- });
    Object.defineProperty(f, '_kids', { value: [] });
    return f;
  });
  getters(DocumentFragment.prototype, ParentNodeGetters);
  methods(DocumentFragment.prototype, ParentNodeMethods);
  methods(DocumentFragment.prototype, {
    getElementById: function (id) { return queryAll(this, '[id="' + cssStr(String(id)) + '"]')[0] || null; },
    getElementsByTagName: function (tag) { tag = String(tag).toLowerCase(); return collectionOf(queryAll(this, tag === '*' ? '*' : tag)); },
    getElementsByClassName: function (cls) { return collectionOf(queryAll(this, '.' + String(cls).trim().split(/\s+/).join('.'))); }
  });
  global.ShadowRoot = makeClass('ShadowRoot', DocumentFragment);
  // host and mode belong to ShadowRoot, not DocumentFragment: a script that subclasses DocumentFragment (the Shady DOM
  // polyfill's own ShadowRoot) assigns this.host, which a getter-only accessor inherited from DocumentFragment would swallow.
  getters(global.ShadowRoot.prototype, { host: { get: function () { return null; } }, mode: { get: function () { return 'open'; } } });

  // --- Document -------------------------------------------------------------
  var Document = makeClass('Document', Node, function Document() { return createDetachedDocument(); });
  var readyState = 'loading', currentScript = null, pageHidden = false; // pageHidden: the tab is in the background (Tab.SetBackground)
  function createDetachedDocument() {
    // used by DOMParser / implementation.createHTMLDocument: a detached <html> element masquerading as a document
    var html = documentWrapper.createElement('html');
    html.appendChild(documentWrapper.createElement('head'));
    html.appendChild(documentWrapper.createElement('body'));
    var d = Object.create(Document.prototype);
    Object.defineProperty(d, ID, { value: html[ID] });
    Object.defineProperty(d, '_detachedRoot', { value: html });
    return d;
  }
  getters(Document.prototype, ParentNodeGetters);
  methods(Document.prototype, ParentNodeMethods);
  getters(Document.prototype, {
    nodeType: { get: function () { return 9; } }, nodeName: { get: function () { return '#document'; } },
    parentNode: { get: function () { return null; } }, ownerDocument: { get: function () { return null; } },
    documentElement: { get: function () { if (this._detachedRoot) return this._detachedRoot; var self = this; return structMemo(this, 'docEl', function () { var kids = childElements(self[ID]); return kids.filter(function (e) { return e.localName === 'html'; })[0] || kids[0] || null; }); } },
    head: { get: function () { var self = this; return structMemo(this, 'head', function () { var h = self.documentElement; return h ? childElements(h[ID]).filter(function (e) { return e.localName === 'head'; })[0] || null : null; }); } },
    body: {
      get: function () { var self = this; return structMemo(this, 'body', function () { var h = self.documentElement; return h ? childElements(h[ID]).filter(function (e) { return e.localName === 'body' || e.localName === 'frameset'; })[0] || null : null; }); },
      set: function (v) { var b = this.body; if (b) b.replaceWith(v); else this.documentElement.appendChild(v); }
    },
    title: {
      get: function () { var t = queryOne(this, 'title'); return t ? t.textContent.replace(/[\t\n\f\r ]+/g, ' ').replace(/^ | $/g, '') : ''; },
      set: function (v) { var t = queryOne(this, 'title'); if (!t) { var h = this.head; if (!h) return; t = h.appendChild(this.createElement('title')); } t.textContent = toStr(v); }
    },
    doctype: { get: function () { return wrapAll(sw.children(this[ID])).filter(function (n) { return n.nodeType === 10; })[0] || null; } },
    readyState: { get: function () { return this === documentWrapper ? readyState : 'complete'; } },
    activeElement: { get: function () { return activeElement && activeElement.isConnected ? activeElement : this.body; } },
    currentScript: { get: function () { return currentScript; } },
    defaultView: { get: function () { return this === documentWrapper ? global : null; } },
    location: { get: function () { return this === documentWrapper ? global.location : null; } },
    URL: { get: function () { return currentURL(); } }, documentURI: { get: function () { return currentURL(); } },
    baseURI: { get: function () { var b = queryOne(this, 'base[href]'); if (b) { try { return new global.URL(b.getAttribute('href'), currentURL()).href; } catch (e) { /* ignore */ } } return currentURL(); } },
    domain: { get: function () { try { return new global.URL(currentURL()).hostname; } catch (e) { return ''; } }, set: function () { } },
    referrer: { get: function () { return ''; } },
    cookie: { get: function () { return sw.cookie() || ''; }, set: function (v) { sw.setCookie(toStr(v)); } },
    lastModified: { get: function () { return new Date().toLocaleString(); } },
    characterSet: { get: function () { return 'UTF-8'; } }, charset: { get: function () { return 'UTF-8'; } }, inputEncoding: { get: function () { return 'UTF-8'; } },
    contentType: { get: function () { return 'text/html'; } }, compatMode: { get: function () { var q = false; try { q = !!sw.quirks(); } catch (e) { /* ignore */ } return q ? 'BackCompat' : 'CSS1Compat'; } },
    hidden: { get: function () { return pageHidden; } }, visibilityState: { get: function () { return pageHidden ? 'hidden' : 'visible'; } },
    designMode: { get: function () { return 'off'; }, set: function () { } },
    dir: { get: function () { var h = this.documentElement; return h ? h.getAttribute('dir') || '' : ''; }, set: function (v) { var h = this.documentElement; if (h) h.setAttribute('dir', toStr(v)); } },
    forms: { get: function () { return this.getElementsByTagName('form'); } },
    images: { get: function () { return this.getElementsByTagName('img'); } },
    links: { get: function () { var self = this; return liveList(HTMLCollection, genCached(function () { return queryAll(self, 'a[href],area[href]'); })); } },
    anchors: { get: function () { var self = this; return liveList(HTMLCollection, genCached(function () { return queryAll(self, 'a[name]'); })); } },
    scripts: { get: function () { return this.getElementsByTagName('script'); } },
    embeds: { get: function () { return this.getElementsByTagName('embed'); } }, plugins: { get: function () { return this.getElementsByTagName('embed'); } },
    styleSheets: { get: function () { var list = queryAll(this, 'style,link[rel~="stylesheet"]').map(function (e) { return e.sheet; }).filter(Boolean); list.item = function (i) { return list[i] || null; }; return list; } },
    fonts: { get: function () { return { ready: Promise.resolve(), check: function () { return true; }, load: function () { return Promise.resolve([]); }, addEventListener: function () { }, removeEventListener: function () { }, status: 'loaded' }; } },
    implementation: { get: function () { return domImplementation; } },
    fullscreenElement: { get: function () { return null; } }, pointerLockElement: { get: function () { return null; } }, pictureInPictureElement: { get: function () { return null; } },
    fullscreenEnabled: { get: function () { return false; } },
    scrollingElement: { get: function () { return this.documentElement; } },
    rootElement: { get: function () { return null; } },
    all: { get: function () { return collectionOf(queryAll(this, '*')); } },
    timeline: { get: function () { return { currentTime: nowMs() }; } }
  });
  // The "template contents owner document": the document every template's content belongs to.
  function templateOwnerDocument() { return templateDocument || (templateDocument = createDetachedDocument()); }
  var domImplementation = {
    createHTMLDocument: function (title) { var d = createDetachedDocument(); if (title !== undefined) d.title = title; return d; },
    createDocument: function () { return createDetachedDocument(); },
    createDocumentType: function () { return null; },
    hasFeature: function () { return true; }
  };
  methods(Document.prototype, {
    createElement: function (tag, options) {
      if (arguments.length === 0) throw new TypeError("Failed to execute 'createElement' on 'Document': 1 argument required, but only 0 present.");
      tag = toStr(tag);
      // an XML Name may start with a letter, '_' or ':' (document.createElement('_') is valid; MediaWiki's VisualEditor uses it)
      if (!/^[A-Za-z_:\u00C0-\uFFFF][^\s\/>\u0000]*$/.test(tag)) throw DOMException("Failed to execute 'createElement' on 'Document': The tag name provided ('" + tag + "') is not a valid name.", 'InvalidCharacterError');
      var lower = tag.toLowerCase();
      var is = options && typeof options === 'object' ? options.is : undefined;
      var id = sw.createElement(lower);
      var inert = this._detachedRoot !== undefined; // an inert document has no custom element registry: nothing is constructed
      if (inert) noUpgrade = true;
      var w;
      try { w = wrap(id); } finally { noUpgrade = false; }
      if (inert) Object.defineProperty(w, INERT_EL, { value: true, configurable: true });
      if (is) slots(w).isValue = String(is); // the "is value" is not an attribute: getAttribute('is') stays null
      hostMutation('create', id);
      return w;
    },
    createElementNS: function (ns, qname, options) {
      qname = toStr(qname);
      var local = qname.indexOf(':') >= 0 ? qname.split(':')[1] : qname;
      if (ns === XHTML_NS || ns === null || ns === '') return this.createElement(local, options);
      var id = sw.createElement(local.toLowerCase());
      var C = ns === SVG_NS ? (local.toLowerCase() === 'svg' ? global.SVGSVGElement : SVG_CLASS[local.toLowerCase()] || SVGElement) : Element;
      var w = makeWrapper(id, C);
      w[NS] = ns; w[ORIG_TAG] = local;
      hostMutation('create', id);
      return w;
    },
    createTextNode: function (data) { return new Text(toStr(data)); },
    createComment: function (data) { return new Comment(toStr(data)); },
    createCDATASection: function (data) { return new Text(toStr(data)); },
    createProcessingInstruction: function (t, d) { return new Comment('?' + t + ' ' + d + '?'); },
    createDocumentFragment: function () { return new DocumentFragment(); },
    createAttribute: function (name) { var a = Object.create(Attr.prototype); var val = ''; Object.defineProperty(a, 'name', { value: String(name).toLowerCase(), enumerable: true }); Object.defineProperty(a, 'ownerElement', { value: null }); Object.defineProperty(a, 'value', { get: function () { return val; }, set: function (v) { val = toStr(v); }, enumerable: true }); return a; },
    createAttributeNS: function (ns, name) { return this.createAttribute(name); },
    createEvent: function (iface) {
      iface = String(iface);
      var map = { Event: 'Event', Events: 'Event', HTMLEvents: 'Event', UIEvent: 'UIEvent', UIEvents: 'UIEvent', MouseEvent: 'MouseEvent', MouseEvents: 'MouseEvent', CustomEvent: 'CustomEvent', KeyboardEvent: 'KeyboardEvent', FocusEvent: 'FocusEvent', TouchEvent: 'TouchEvent', MessageEvent: 'MessageEvent', InputEvent: 'InputEvent', CompositionEvent: 'CompositionEvent', DragEvent: 'DragEvent', WheelEvent: 'WheelEvent', StorageEvent: 'StorageEvent', HashChangeEvent: 'HashChangeEvent', PopStateEvent: 'PopStateEvent', ErrorEvent: 'ErrorEvent', ProgressEvent: 'ProgressEvent', AnimationEvent: 'AnimationEvent', TransitionEvent: 'TransitionEvent', TextEvent: 'CompositionEvent' };
      var C = global[map[iface]];
      if (!C) throw DOMException("Failed to execute 'createEvent' on 'Document': The provided event type ('" + iface + "') is invalid.", 'NotSupportedError');
      var ev = new C('');
      ev.type = ''; ev._dispatching = false;
      return ev;
    },
    createRange: function () { return new global.Range(); },
    createTreeWalker: function (root, whatToShow, filter) { return new global.TreeWalker(root, whatToShow, filter); },
    createNodeIterator: function (root, whatToShow, filter) { return new global.NodeIterator(root, whatToShow, filter); },
    getElementById: function (id) { id = toStr(id); if (this._detachedRoot || this !== documentWrapper) return queryAll(this, '[id="' + cssStr(id) + '"]')[0] || null; return wrap(sw.getElementById(id)); },
    getElementsByName: function (name) { name = toStr(name); var self = this; return liveList(NodeList, genCached(function () { return self === documentWrapper ? wrapAll(sw.elementsByName(name)) : queryAll(self, '[name="' + cssStr(name) + '"]'); })); },
    importNode: function (node, deep) { requireNode(node, 'importNode'); return node.cloneNode(!!deep); },
    adoptNode: function (node) { requireNode(node, 'adoptNode'); if (node.parentNode) node.parentNode.removeChild(node); return node; },
    hasFocus: function () { return true; },
    elementFromPoint: function (x, y) { var p = scrollPos(); if (host('elementFromPoint')) return wrap(sw.elementFromPoint((+x || 0) + p[0], (+y || 0) + p[1])); return this.body; },
    elementsFromPoint: function (x, y) { var e = this.elementFromPoint(x, y); var out = []; for (; e; e = e.parentElement) out.push(e); return out; },
    caretRangeFromPoint: function () { return null; }, caretPositionFromPoint: function () { return null; },
    getSelection: function () { return global.getSelection(); },
    execCommand: function () { return false; }, queryCommandSupported: function () { return false; }, queryCommandEnabled: function () { return false; }, queryCommandState: function () { return false; }, queryCommandValue: function () { return ''; },
    open: function () { return this; }, close: function () { },
    write: function () { var html = slice.call(arguments).join(''); var target = this.body || this.documentElement; if (target) target.insertAdjacentHTML('beforeend', html); },
    writeln: function () { this.write(slice.call(arguments).join('') + '\n'); },
    exitFullscreen: function () { return Promise.resolve(); }, exitPointerLock: function () { },
    startViewTransition: function (cb) { var p = Promise.resolve().then(function () { if (cb) return cb(); }); return { ready: p, finished: p, updateCallbackDone: p, skipTransition: function () { } }; }
  });
  // detached documents resolve queries against their root element
  (function () {
    var wrapDetached = function (name) {
      var orig = Document.prototype[name];
      method(Document.prototype, name, function () { if (this._detachedRoot) return orig.apply(this._detachedRoot, arguments); return orig.apply(this, arguments); });
    };
    ['querySelector', 'querySelectorAll', 'getElementsByTagName', 'getElementsByClassName'].forEach(wrapDetached);
    var cd = Object.getOwnPropertyDescriptor(ParentNodeGetters, 'children');
    getters(Document.prototype, { children: { get: function () { return this._detachedRoot ? collectionOf([this._detachedRoot]) : cd.get.call(this); } } });
  })();
  global.HTMLDocument = Document; global.XMLDocument = Document;

  // ---- Range / TreeWalker / NodeIterator / Selection (minimal, enough for libs that feature-detect)
  function Range() { this.startContainer = this.endContainer = documentWrapper; this.startOffset = this.endOffset = 0; this.collapsed = true; this.commonAncestorContainer = documentWrapper; }
  Range.prototype = {
    constructor: Range,
    setStart: function (n, o) { this.startContainer = n; this.startOffset = o; this._upd(); }, setEnd: function (n, o) { this.endContainer = n; this.endOffset = o; this._upd(); },
    setStartBefore: function (n) { this.setStart(n.parentNode, idx(n)); }, setStartAfter: function (n) { this.setStart(n.parentNode, idx(n) + 1); }, setEndBefore: function (n) { this.setEnd(n.parentNode, idx(n)); }, setEndAfter: function (n) { this.setEnd(n.parentNode, idx(n) + 1); },
    selectNode: function (n) { this.setStart(n.parentNode, idx(n)); this.setEnd(n.parentNode, idx(n) + 1); }, selectNodeContents: function (n) { this.setStart(n, 0); this.setEnd(n, n.nodeType === 3 ? n.length : n.childNodes.length); },
    collapse: function (toStart) { if (toStart) { this.endContainer = this.startContainer; this.endOffset = this.startOffset; } else { this.startContainer = this.endContainer; this.startOffset = this.endOffset; } this.collapsed = true; },
    cloneRange: function () { var r = new Range(); r.setStart(this.startContainer, this.startOffset); r.setEnd(this.endContainer, this.endOffset); return r; },
    cloneContents: function () { return documentWrapper.createDocumentFragment(); }, extractContents: function () { return documentWrapper.createDocumentFragment(); }, deleteContents: function () { },
    insertNode: function (n) { var c = this.startContainer; if (c.nodeType === 3) c.parentNode.insertBefore(n, c.splitText(this.startOffset)); else c.insertBefore(n, c.childNodes[this.startOffset] || null); },
    surroundContents: function (n) { this.insertNode(n); }, detach: function () { },
    createContextualFragment: function (html) { var tmp = documentWrapper.createElement('template'); tmp.innerHTML = toStr(html); return tmp.content; },
    getBoundingClientRect: function () { return new DOMRect(0, 0, 0, 0); }, getClientRects: function () { return []; },
    compareBoundaryPoints: function () { return 0; }, isPointInRange: function () { return false; }, intersectsNode: function () { return false; },
    toString: function () { return ''; },
    _upd: function () { this.collapsed = this.startContainer === this.endContainer && this.startOffset === this.endOffset; var a = this.startContainer, b = this.endContainer; while (a && !a.contains(b)) a = a.parentNode; this.commonAncestorContainer = a || documentWrapper; }
  };
  function idx(n) { return n.parentNode ? itemsOf(n.parentNode.childNodes).indexOf(n) : 0; }
  constant(Range, 'START_TO_START', 0); constant(Range, 'START_TO_END', 1); constant(Range, 'END_TO_END', 2); constant(Range, 'END_TO_START', 3);
  setTag(Range, 'Range'); global.Range = Range; global.StaticRange = Range;
  var NodeFilter = { FILTER_ACCEPT: 1, FILTER_REJECT: 2, FILTER_SKIP: 3, SHOW_ALL: 0xFFFFFFFF, SHOW_ELEMENT: 1, SHOW_ATTRIBUTE: 2, SHOW_TEXT: 4, SHOW_COMMENT: 128, SHOW_DOCUMENT: 256, SHOW_DOCUMENT_FRAGMENT: 1024 };
  global.NodeFilter = NodeFilter;
  function accepts(walker, n) {
    if (!((1 << (n.nodeType - 1)) & walker.whatToShow)) return 3;
    var f = walker.filter; if (!f) return 1;
    var r = typeof f === 'function' ? f(n) : f.acceptNode(n);
    return r === undefined ? 1 : r;
  }
  function TreeWalker(root, whatToShow, filter) { this.root = root; this.whatToShow = whatToShow === undefined ? 0xFFFFFFFF : whatToShow; this.filter = filter || null; this.currentNode = root; }
  TreeWalker.prototype = {
    constructor: TreeWalker,
    _walkChild: function (first) { var n = first ? this.currentNode.firstChild : this.currentNode.lastChild; while (n) { var r = accepts(this, n); if (r === 1) { this.currentNode = n; return n; } if (r === 3) { var c = first ? n.firstChild : n.lastChild; if (c) { n = c; continue; } } while (n) { var s = first ? n.nextSibling : n.previousSibling; if (s) { n = s; break; } n = n.parentNode; if (!n || n === this.root || n === this.currentNode) return null; } } return null; },
    firstChild: function () { return this._walkChild(true); }, lastChild: function () { return this._walkChild(false); },
    parentNode: function () { var n = this.currentNode; while (n && n !== this.root) { n = n.parentNode; if (n && accepts(this, n) === 1) { this.currentNode = n; return n; } } return null; },
    _sibling: function (next) { var n = this.currentNode; if (n === this.root) return null; while (true) { var s = next ? n.nextSibling : n.previousSibling; while (s) { var r = accepts(this, s); if (r === 1) { this.currentNode = s; return s; } var c = next ? s.firstChild : s.lastChild; if (r === 2 || !c) s = next ? s.nextSibling : s.previousSibling; else s = c; } n = n.parentNode; if (!n || n === this.root || accepts(this, n) === 1) return null; } },
    nextSibling: function () { return this._sibling(true); }, previousSibling: function () { return this._sibling(false); },
    nextNode: function () { var n = this.currentNode; while (true) { var r = n === this.currentNode ? 1 : accepts(this, n); var c = r !== 2 ? n.firstChild : null; if (c) n = c; else { while (n && n !== this.root && !n.nextSibling) n = n.parentNode; if (!n || n === this.root) return null; n = n.nextSibling; } if (accepts(this, n) === 1) { this.currentNode = n; return n; } } },
    previousNode: function () { var n = this.currentNode; while (n && n !== this.root) { var s = n.previousSibling; if (s) { n = s; while (n.lastChild && accepts(this, n) !== 2) n = n.lastChild; } else { n = n.parentNode; if (!n || n === this.root) return null; } if (accepts(this, n) === 1) { this.currentNode = n; return n; } } return null; }
  };
  setTag(TreeWalker, 'TreeWalker'); global.TreeWalker = TreeWalker;
  function NodeIterator(root, whatToShow, filter) { this.root = root; this.whatToShow = whatToShow === undefined ? 0xFFFFFFFF : whatToShow; this.filter = filter || null; this.referenceNode = root; this.pointerBeforeReferenceNode = true; this._list = null; this._i = 0; }
  NodeIterator.prototype = {
    constructor: NodeIterator,
    _all: function () { var out = [], self = this; (function walk(n) { if (accepts(self, n) === 1) out.push(n); var kids = itemsOf(n.childNodes); kids.forEach(walk); })(this.root); return out; },
    nextNode: function () { if (!this._list) this._list = this._all(); var n = this._list[this._i] || null; if (n) { this._i++; this.referenceNode = n; } return n; },
    previousNode: function () { if (!this._list || this._i === 0) return null; this._i--; return this._list[this._i]; },
    detach: function () { }
  };
  setTag(NodeIterator, 'NodeIterator'); global.NodeIterator = NodeIterator;
  var selectionStub = { anchorNode: null, anchorOffset: 0, focusNode: null, focusOffset: 0, isCollapsed: true, rangeCount: 0, type: 'None', getRangeAt: function () { throw DOMException('Index or size is negative or greater than the allowed amount', 'IndexSizeError'); }, addRange: function () { }, removeAllRanges: function () { }, removeRange: function () { }, collapse: function () { }, collapseToStart: function () { }, collapseToEnd: function () { }, selectAllChildren: function () { }, extend: function () { }, setBaseAndExtent: function () { }, deleteFromDocument: function () { }, containsNode: function () { return false; }, toString: function () { return ''; } };
  global.getSelection = function () { return selectionStub; };
  global.Selection = function Selection() { illegal(); };

  // ==== 6. Reflected attributes (generated) ================================
  // "prop" or "prop=attr" with a kind suffix: ":b" boolean, ":i" integer
  // (":i=default"), ":u" URL-resolved, ":e" enumerated-string, default string.
  // Each reflected property lives on the interface(s) that own it, as in the HTML spec: `'href' in div` and `'data' in el` are
  // false, `'labels' in input` is true. Scripts tell native properties from their own with exactly these probes (Polymer
  // Resin sanitises every property that exists on a freshly made element, and must see none on a custom element).
  // Targets are tag names, HTML*Element interface names, or '*' for HTMLElement itself.
  var REFLECT = [
    ['*', 'title lang dir=dir:e hidden:b tabIndex=tabindex:i=-1 accessKey=accesskey draggable=draggable:tb spellcheck=spellcheck:tb translate=translate:yb contentEditable=contenteditable:e inert:b autofocus:b role=role:n enterKeyHint=enterkeyhint inputMode=inputmode virtualKeyboardPolicy=virtualkeyboardpolicy autocapitalize nonce:skip slot:skip part:skip id:skip className:skip'],
    ['a area link base', 'href=href:n'],
    ['img script iframe embed source track input HTMLMediaElement', 'src=src:n'],
    ['a link script source style button input ol li ul embed object', 'type'],
    ['a img iframe form input select textarea button fieldset output object meta map slot embed details', 'name'],
    ['img area input', 'alt'],
    ['input textarea', 'placeholder'],
    ['input select textarea button fieldset optgroup option link', 'disabled:b'],
    ['option', 'selected:b'],
    ['label', 'htmlFor=for'],
    ['form', 'action method noValidate=novalidate:b enctype encoding=enctype acceptCharset=accept-charset'],
    ['a area base form', 'target'],
    ['a area link', 'rel'],
    ['a area', 'download ping'],
    ['a area link', 'hreflang'],
    ['img canvas video iframe embed object input source', 'width:i height:i'],
    ['img iframe', 'loading'],
    ['img source', 'srcset sizes'],
    ['meta', 'content httpEquiv=http-equiv scheme'],
    ['meta script', 'charset'],
    ['script', 'async:b defer:b'],
    ['img script link HTMLMediaElement', 'crossOrigin=crossorigin:n'],
    ['script link', 'integrity'],
    ['a area img iframe link script', 'referrerPolicy=referrerpolicy'],
    ['input select textarea form', 'autocomplete'],
    ['input select textarea', 'required:b'],
    ['input textarea', 'readOnly=readonly:b maxLength=maxlength:i=-1 minLength=minlength:i=-1 dirName=dirname'],
    ['input select', 'multiple:b'],
    ['input meter', 'min'],
    ['input meter progress', 'max'],
    ['input', 'step pattern size:i=20 accept capture'],
    ['select', 'size:i=0'],
    ['textarea', 'rows:i=2 cols:i=20 wrap'],
    ['td th', 'colSpan=colspan:i=1 rowSpan=rowspan:i=1 headers abbr axis'],
    ['th', 'scope'],
    ['details dialog', 'open:b'],
    ['option optgroup track', 'label'],
    ['track', 'default:b kind srclang'],
    ['blockquote q ins del', 'cite'],
    ['ins del time', 'dateTime=datetime'],
    ['object', 'data declare:b standby'],
    ['HTMLMediaElement', 'controls:b autoplay:b loop:b muted:b preload'],
    ['video', 'poster playsInline=playsinline:b'],
    ['img', 'isMap=ismap:b decoding longDesc=longdesc lowsrc'],
    ['img object', 'useMap=usemap border vspace:i hspace:i'],
    ['area', 'shape coords noHref=nohref:b'],
    ['link source style', 'media'],
    ['iframe', 'sandbox allow allowFullscreen=allowfullscreen:b scrolling frameBorder=frameborder marginHeight=marginheight marginWidth=marginwidth'],
    ['div p hr table caption col colgroup tbody thead tfoot tr td th iframe img object embed legend input h1 h2 h3 h4 h5 h6', 'align'],
    ['tr td th tbody thead tfoot col colgroup', 'vAlign=valign char charOff=charoff'],
    ['body table tr td th', 'bgColor=bgcolor'],
    ['table', 'border cellPadding=cellpadding cellSpacing=cellspacing summary'],
    ['button input', 'formAction=formaction formMethod=formmethod formTarget=formtarget formNoValidate=formnovalidate:b formEnctype=formenctype'],
    ['li button data param meter progress', 'value=value:v'],
    ['ol', 'start:i=1 reversed:b'],
    ['col colgroup', 'span:i=1'],
    ['td th', 'noWrap=nowrap:b'],
    ['ul ol dl menu', 'compact:b'],
    ['hr', 'noShade=noshade:b'],
    ['html', 'version'],
    ['meter', 'optimum:i low:i high:i'],
    ['img link script iframe', 'fetchPriority=fetchpriority'],
    ['script link style', 'blocking']
  ];
  // Props that specialised prototypes already own (never clobber those).
  var _unusedProtos = [InputP, TextAreaP, SelectP, OptionP, FormP, global.HTMLButtonElement.prototype, global.HTMLAnchorElement.prototype, global.HTMLAreaElement.prototype, global.HTMLLinkElement.prototype, global.HTMLBaseElement.prototype, ImgP, global.HTMLCanvasElement.prototype, global.HTMLDetailsElement.prototype, global.HTMLDialogElement.prototype, global.HTMLProgressElement.prototype, global.HTMLMeterElement.prototype, global.HTMLTimeElement.prototype, global.HTMLLIElement.prototype, global.HTMLDataElement.prototype, global.HTMLOutputElement.prototype, global.HTMLScriptElement.prototype, global.HTMLIFrameElement.prototype, global.HTMLSourceElement.prototype, global.HTMLEmbedElement.prototype, global.HTMLTrackElement.prototype, global.HTMLMediaElement.prototype, global.HTMLObjectElement.prototype];
  function reflectDescriptor(attr, kind, dflt) {
    switch (kind) {
      case 'b': return { get: function () { return !!sw.hasAttr(this[ID], attr); }, set: function (v) { if (v) this.setAttribute(attr, ''); else this.removeAttribute(attr); } };
      case 'i': return { get: function () { var v = parseInt(sw.getAttr(this[ID], attr), 10); return isNaN(v) ? (dflt === undefined ? 0 : +dflt) : v; }, set: function (v) { this.setAttribute(attr, String(parseInt(v, 10) || 0)); } };
      case 'tb': return { get: function () { return sw.getAttr(this[ID], attr) === 'true'; }, set: function (v) { this.setAttribute(attr, v ? 'true' : 'false'); } };
      case 'yb': return { get: function () { return sw.getAttr(this[ID], attr) !== 'no'; }, set: function (v) { this.setAttribute(attr, v ? 'yes' : 'no'); } };
      case 'e': return { get: function () { var v = sw.getAttr(this[ID], attr); return v === null || v === undefined ? (attr === 'contenteditable' ? 'inherit' : '') : v; }, set: function (v) { this.setAttribute(attr, toStr(v)); } };
      case 'n': return { get: function () { var v = sw.getAttr(this[ID], attr); return v === undefined ? null : v; }, set: function (v) { this.setAttribute(attr, toStr(v)); } };
      case 'v': return { get: function () { var v = sw.getAttr(this[ID], attr); return v === null || v === undefined ? '' : v; }, set: function (v) { this.setAttribute(attr, toStr(v)); } };
      default: return { get: function () { var v = sw.getAttr(this[ID], attr); return v === null || v === undefined ? '' : v; }, set: function (v) { this.setAttribute(attr, toStr(v)); } };
    }
  }
  REFLECT.forEach(function (entry) {
    var targets = [];
    entry[0].split(' ').forEach(function (t) {
      var C = t === '*' ? HTMLElement : /^HTML/.test(t) ? global[t] : TAG_CLASS[t];
      if (C && targets.indexOf(C) < 0) targets.push(C);
    });
    entry[1].split(' ').forEach(function (spec) {
      var m = /^([A-Za-z]+)(?:=([a-z-]+))?(?::([a-z]+)(?:=(-?\d+))?)?$/.exec(spec);
      if (!m) throw new Error('bad reflect spec ' + spec);
      var prop = m[1], attr = m[2] || prop.toLowerCase(), kind = m[3] || 's', dflt = m[4];
      if (kind === 'skip') return;
      var d = reflectDescriptor(attr, kind, dflt);
      targets.forEach(function (C) {
        if (hasOwn.call(C.prototype, prop)) return;
        def(C.prototype, prop, { get: d.get, set: d.set, enumerable: true });
      });
      // SVG elements share the string reflectors for the few that make sense
      if (/^(tabIndex|role|hidden|title|lang|dataset|type|href|target|download|name)$/.test(prop) && !hasOwn.call(SVGElement.prototype, prop)) def(SVGElement.prototype, prop, { get: d.get, set: d.set, enumerable: true });
    });
  });
  // input/textarea/select own live `value` accessors above; the others in the `value` row reflect the attribute.
  _unusedProtos.length = 0;
  getters(Element.prototype, { role: Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'role') });
  // ARIA reflection: ariaLabel <-> aria-label etc.
  ('atomic autoComplete busy checked colCount colIndex colSpan controls current describedBy description details disabled errorMessage expanded flowTo hasPopup hidden invalid keyShortcuts label labelledBy level live modal multiLine multiSelectable orientation owns placeholder posInSet pressed readOnly relevant required roleDescription rowCount rowIndex rowSpan selected setSize sort valueMax valueMin valueNow valueText').split(' ').forEach(function (n) {
    var attr = 'aria-' + n.toLowerCase();
    def(Element.prototype, 'aria' + n[0].toUpperCase() + n.slice(1), { get: function () { var v = sw.getAttr(this[ID], attr); return v === undefined ? null : v; }, set: function (v) { if (v === null) this.removeAttribute(attr); else this.setAttribute(attr, toStr(v)); }, enumerable: true });
  });
  defineHandlerProps(Element.prototype, COMMON_EVENTS);
  defineHandlerProps(Document.prototype, COMMON_EVENTS.concat(['DOMContentLoaded', 'readystatechange', 'visibilitychange', 'fullscreenchange', 'pointerlockchange', 'selectionchange']));
  defineHandlerProps(global, COMMON_EVENTS.concat(WINDOW_EVENTS));

  // ==== 7. Observers =======================================================
  // true when every registered observer is the platform media observer (media_prelude.js), which only acts on
  // <video>/<audio>/<track>/<source> nodes and on child changes inside media elements
  function onlyMediaObservers() { for (var i = 0; i < observers.length; i++) if (!observers[i].__swMedia) return false; return true; }
  var observers = [];          // every MutationObserver with at least one observe()
  var pendingObservers = [];   // observers with queued records
  var moFlushScheduled = false;
  function MutationRecord(type, target) { this.type = type; this.target = target; this.addedNodes = nodeListOf([]); this.removedNodes = nodeListOf([]); this.previousSibling = null; this.nextSibling = null; this.attributeName = null; this.attributeNamespace = null; this.oldValue = null; }
  setTag(MutationRecord, 'MutationRecord'); global.MutationRecord = MutationRecord;
  function recordMutation(target, type, info) {
    if (!observers.length || !target || target.nodeType === 11) return;
    var interested = null;
    for (var node = target; node; node = node.parentNode) {
      for (var i = 0; i < observers.length; i++) {
        var ob = observers[i];
        for (var j = 0; j < ob._targets.length; j++) {
          var t = ob._targets[j];
          if (t.node !== node) continue;
          if (node !== target && !t.options.subtree) continue;
          var o = t.options;
          if (type === 'childList' && !o.childList) continue;
          if (type === 'attributes' && !(o.attributes || o.attributeOldValue || o.attributeFilter)) continue;
          if (type === 'attributes' && o.attributeFilter && o.attributeFilter.indexOf(info.attributeName) < 0) continue;
          if (type === 'characterData' && !(o.characterData || o.characterDataOldValue)) continue;
          var rec = new MutationRecord(type, target);
          if (info.addedNodes) rec.addedNodes = nodeListOf(info.addedNodes);
          if (info.removedNodes) rec.removedNodes = nodeListOf(info.removedNodes);
          rec.previousSibling = info.previousSibling || null; rec.nextSibling = info.nextSibling || null;
          if (type === 'attributes') { rec.attributeName = info.attributeName; if (o.attributeOldValue) rec.oldValue = info.oldValue; }
          if (type === 'characterData' && o.characterDataOldValue) rec.oldValue = info.oldValue;
          if (!interested) interested = new Map();
          if (!interested.has(ob)) interested.set(ob, rec);
        }
      }
    }
    if (!interested) return;
    interested.forEach(function (rec, ob) {
      ob._queue.push(rec);
      if (pendingObservers.indexOf(ob) < 0) pendingObservers.push(ob);
    });
    if (!moFlushScheduled) { moFlushScheduled = true; queueMicrotaskImpl(flushMutationObservers); }
  }
  function flushMutationObservers() {
    moFlushScheduled = false;
    var list = pendingObservers; pendingObservers = [];
    for (var i = 0; i < list.length; i++) {
      var ob = list[i];
      var recs = ob._queue; ob._queue = [];
      if (recs.length) { try { ob._cb.call(ob, recs, ob); } catch (e) { reportError(e); } }
    }
  }
  function MutationObserver(cb) {
    if (!(this instanceof MutationObserver)) throw new TypeError("Constructor MutationObserver requires 'new'");
    if (typeof cb !== 'function') throw new TypeError("Failed to construct 'MutationObserver': The callback provided as parameter 1 is not a function.");
    this._cb = cb; this._targets = []; this._queue = [];
  }
  MutationObserver.prototype = {
    constructor: MutationObserver,
    observe: function (node, options) {
      requireNode(node, 'observe');
      options = Object.assign({}, options || {});
      if (options.attributeOldValue || options.attributeFilter) options.attributes = options.attributes === undefined ? true : options.attributes;
      if (options.characterDataOldValue) options.characterData = options.characterData === undefined ? true : options.characterData;
      if (!options.childList && !options.attributes && !options.characterData) throw new TypeError("Failed to execute 'observe' on 'MutationObserver': The options object must set at least one of 'attributes', 'characterData', or 'childList' to true.");
      for (var i = 0; i < this._targets.length; i++) if (this._targets[i].node === node) { this._targets[i].options = options; return; }
      this._targets.push({ node: node, options: options });
      if (observers.indexOf(this) < 0) observers.push(this);
    },
    disconnect: function () { this._targets = []; this._queue = []; var i = observers.indexOf(this); if (i >= 0) observers.splice(i, 1); },
    takeRecords: function () { var r = this._queue; this._queue = []; return r; }
  };
  setTag(MutationObserver, 'MutationObserver'); global.MutationObserver = MutationObserver; global.WebKitMutationObserver = MutationObserver;

  // IntersectionObserver / ResizeObserver: report every observed element once, as visible.
  // Notifications are batched the way a browser's rendering step delivers them: the observe() calls of a task
  // are answered by one timer that measures every target first (a single layout, since nothing changes the tree
  // between the measurements) and then calls each observer once with its entries. One timer per target made a
  // page that observes fifty links lay out fifty times, every callback's DOM change invalidating the next.
  var obsPending = [], obsScheduled = false;
  function obsQueue(obs, el, make) {
    obsPending.push({ obs: obs, el: el, make: make });
    if (!obsScheduled) { obsScheduled = true; setTimeoutImpl(obsFlush, [], 0, false); }
  }
  function obsFlush() {
    obsScheduled = false;
    var batch = obsPending; obsPending = [];
    var groups = [], i, j;
    for (i = 0; i < batch.length; i++) {
      var p = batch[i];
      if (p.obs._targets.indexOf(p.el) < 0) continue;
      var rect = p.el.getBoundingClientRect ? p.el.getBoundingClientRect() : new DOMRect();
      for (j = 0; j < groups.length && groups[j].obs !== p.obs; j++) {}
      if (j === groups.length) groups.push({ obs: p.obs, entries: [] });
      groups[j].entries.push(p.make(p.el, rect));
    }
    for (i = 0; i < groups.length; i++) {
      var g = groups[i], live = [];
      for (j = 0; j < g.entries.length; j++) if (g.obs._targets.indexOf(g.entries[j].target) >= 0) live.push(g.entries[j]);
      if (live.length) { try { g.obs._cb(live, g.obs); } catch (e) { reportError(e); } }
    }
  }
  function IntersectionObserver(cb, options) {
    if (typeof cb !== 'function') throw new TypeError('callback must be a function');
    this._cb = cb; this._targets = []; options = options || {};
    this.root = options.root || null; this.rootMargin = options.rootMargin || '0px 0px 0px 0px'; this.thresholds = [].concat(options.threshold === undefined ? 0 : options.threshold);
  }
  IntersectionObserver.prototype = {
    constructor: IntersectionObserver,
    observe: function (el) {
      if (this._targets.indexOf(el) >= 0) return;
      this._targets.push(el);
      obsQueue(this, el, function (el, rect) {
        return { target: el, time: nowMs(), isIntersecting: true, intersectionRatio: 1, boundingClientRect: rect, intersectionRect: rect, rootBounds: new DOMRect(0, 0, viewport().width, viewport().height), isVisible: true };
      });
    },
    unobserve: function (el) { var i = this._targets.indexOf(el); if (i >= 0) this._targets.splice(i, 1); },
    disconnect: function () { this._targets = []; },
    takeRecords: function () { return []; }
  };
  setTag(IntersectionObserver, 'IntersectionObserver'); global.IntersectionObserver = IntersectionObserver;
  global.IntersectionObserverEntry = function IntersectionObserverEntry() { illegal(); };
  function ResizeObserver(cb) {
    if (typeof cb !== 'function') throw new TypeError('callback must be a function');
    this._cb = cb; this._targets = [];
  }
  ResizeObserver.prototype = {
    constructor: ResizeObserver,
    observe: function (el) {
      if (this._targets.indexOf(el) >= 0) return;
      this._targets.push(el);
      obsQueue(this, el, function (el, rect) {
        var size = [{ inlineSize: rect.width, blockSize: rect.height }];
        return { target: el, contentRect: rect, borderBoxSize: size, contentBoxSize: size, devicePixelContentBoxSize: size };
      });
    },
    unobserve: IntersectionObserver.prototype.unobserve,
    disconnect: IntersectionObserver.prototype.disconnect
  };
  setTag(ResizeObserver, 'ResizeObserver'); global.ResizeObserver = ResizeObserver;
  global.PerformanceObserver = function PerformanceObserver(cb) { this._cb = cb; };
  global.PerformanceObserver.prototype = { observe: function () { }, disconnect: function () { }, takeRecords: function () { return []; } };
  global.PerformanceObserver.supportedEntryTypes = [];
  global.ReportingObserver = function ReportingObserver() { }; global.ReportingObserver.prototype = { observe: function () { }, disconnect: function () { }, takeRecords: function () { return []; } };

  // ==== 8. Timers ==========================================================
  // Heap owned by JS; Go pumps __swRunTimers(nowMs). Ids are increasing ints.
  var timers = [];        // sorted by {when, seq}
  var timerSeq = 0, timerIdSeq = 0, lastNow = 0;
  function nowMs() { var n = 0; try { n = +sw.now() || 0; } catch (e) { /* ignore */ } if (n > lastNow) lastNow = n; return lastNow; }
  function timerInsert(t) {
    var lo = 0, hi = timers.length;
    while (lo < hi) { var mid = (lo + hi) >> 1; var m = timers[mid]; if (m.when < t.when || (m.when === t.when && m.seq < t.seq)) lo = mid + 1; else hi = mid; }
    timers.splice(lo, 0, t);
  }
  function setTimeoutImpl(fn, args, delay, repeat) {
    delay = +delay; if (!(delay >= 0)) delay = 0; if (delay > 2147483647) delay = 1;
    var t = { id: ++timerIdSeq, when: nowMs() + delay, seq: ++timerSeq, fn: fn, args: args || [], interval: repeat ? Math.max(delay, 0) : -1, cancelled: false };
    timerInsert(t);
    return t.id;
  }
  function clearTimer(id) {
    id = +id;
    for (var i = 0; i < timers.length; i++) if (timers[i].id === id) { timers[i].cancelled = true; timers.splice(i, 1); return; }
    if (runningTimer && runningTimer.id === id) runningTimer.cancelled = true;
  }
  var runningTimer = null;
  function runTimer(t) {
    runningTimer = t;
    try {
      if (typeof t.fn === 'function') t.fn.apply(global, t.args);
      else if (typeof t.fn === 'string') (0, eval)(t.fn);
    } catch (e) { reportError(e); }
    runningTimer = null;
  }
  function runTimers(now, withBg, budget) {
    now = +now; if (!(now >= 0)) now = nowMs(); if (now > lastNow) lastNow = now;
    // budget (ms, 0 = none): stop starting timers once it is spent; the rest stay due for the next pump
    var t0 = Date.now(); budget = +budget || 0;
    // Only timers that were pending when this pass started run now; timers armed
    // by callbacks (and re-armed intervals) wait for the next pump so microtasks
    // get flushed between macrotasks, as in a browser event loop.
    var startSeq = timerSeq;
    while (timers.length && timers[0].when <= now && timers[0].seq <= startSeq) {
      if (budget > 0 && Date.now() - t0 >= budget) return 0;
      var t = timers.shift();
      runTimer(t);
      if (t.interval >= 0 && !t.cancelled) {
        var step = Math.max(t.interval, 1);
        t.when = t.when + step; if (t.when <= now) t.when = now + step;
        t.seq = ++timerSeq; timerInsert(t);
      }
    }
    // background timers (media clock ticks) never hold the pump open
    for (var bi = 0; bi < timers.length; bi++) if (withBg || !timers[bi].bg) return Math.max(0, timers[bi].when - now);
    return -1;
  }
  Object.defineProperty(global, '__swBgTimeout', { value: function (fn, delay) {
    var id = setTimeoutImpl(fn, [], delay, false);
    for (var i = 0; i < timers.length; i++) if (timers[i].id === id) { timers[i].bg = true; break; }
    return id;
  } });
  function timerArgs(args) { return slice.call(args, 2); }
  global.setTimeout = function setTimeout(fn, delay) { return setTimeoutImpl(fn, timerArgs(arguments), delay, false); };
  global.setInterval = function setInterval(fn, delay) { return setTimeoutImpl(fn, timerArgs(arguments), delay, true); };
  global.clearTimeout = function clearTimeout(id) { clearTimer(id); };
  global.clearInterval = global.clearTimeout;
  global.setImmediate = function setImmediate(fn) { return setTimeoutImpl(fn, slice.call(arguments, 1), 0, false); };
  global.clearImmediate = global.clearTimeout;
  global.requestAnimationFrame = function requestAnimationFrame(cb) { return setTimeoutImpl(function () { cb(nowMs()); }, [], 16, false); };
  global.cancelAnimationFrame = global.clearTimeout;
  global.webkitRequestAnimationFrame = global.requestAnimationFrame; global.webkitCancelAnimationFrame = global.cancelAnimationFrame;
  global.requestIdleCallback = function requestIdleCallback(cb, opts) { var start = nowMs(); return setTimeoutImpl(function () { cb({ didTimeout: false, timeRemaining: function () { return Math.max(0, 50 - (nowMs() - start)); } }); }, [], opts && opts.timeout ? Math.min(opts.timeout, 1) : 1, false); };
  global.cancelIdleCallback = global.clearTimeout;
  var microtaskQueue = [];
  function queueMicrotaskImpl(fn) {
    Promise.resolve().then(function () { try { fn(); } catch (e) { reportError(e); } });
  }
  global.queueMicrotask = function queueMicrotask(fn) { if (typeof fn !== 'function') throw new TypeError("Failed to execute 'queueMicrotask': parameter 1 is not of type 'Function'."); queueMicrotaskImpl(fn); };

  // MessageChannel: port.postMessage delivers through a 0 ms timer (React scheduler).
  function MessagePort() { EventTarget.call(this); this._other = null; this._started = false; this._queue = []; }
  extend(MessagePort, EventTarget);
  methods(MessagePort.prototype, {
    postMessage: function (data) {
      var other = this._other; if (!other) return;
      setTimeoutImpl(function () {
        var ev = new global.MessageEvent('message', { data: data, origin: '', ports: [] });
        dispatch(other, ev);
      }, [], 0, false);
    },
    start: function () { this._started = true; }, close: function () { this._other = null; }
  });
  defineHandlerProps(MessagePort.prototype, ['message', 'messageerror']);
  setTag(MessagePort, 'MessagePort'); global.MessagePort = MessagePort;
  function MessageChannel() { if (!(this instanceof MessageChannel)) throw new TypeError("Constructor MessageChannel requires 'new'"); this.port1 = new MessagePort(); this.port2 = new MessagePort(); this.port1._other = this.port2; this.port2._other = this.port1; }
  setTag(MessageChannel, 'MessageChannel'); global.MessageChannel = MessageChannel;
  global.postMessage = function postMessage(data) { setTimeoutImpl(function () { dispatch(global, new global.MessageEvent('message', { data: data, origin: docOriginStr(), source: global })); }, [], 0, false); };

  // ==== 9. Platform ========================================================
  // ---- window identity
  def(global, 'window', { value: global, enumerable: true, writable: false });
  ['self', 'top', 'parent', 'frames', 'globalThis'].forEach(function (k) { def(global, k, { get: function () { return global; }, set: function () { }, enumerable: true }); });
  def(global, 'frameElement', { value: null });
  def(global, 'opener', { value: null, writable: true });
  def(global, 'length', { value: 0, writable: true });
  def(global, 'closed', { value: false, writable: true });
  def(global, 'name', { value: '', writable: true });
  def(global, 'origin', { get: function () { return docOriginStr(); } });
  def(global, 'isSecureContext', { get: function () { var u = currentURL(); return /^(https|sweef):/.test(u) || /^http:\/\/(localhost|127\.0\.0\.1|\[::1\]|[^/:]*\.localhost)(:|\/|$)/.test(u); } });
  def(global, 'crossOriginIsolated', { get: function () { return host('coi') ? !!sw.coi() : false; }, enumerable: true });
  function Window() { illegal(); } setTag(Window, 'Window'); global.Window = Window;
  Object.setPrototypeOf(Window.prototype, EventTarget.prototype);
  (function () { // make `window` an EventTarget without changing globalThis's prototype chain surprises
    var proto = Object.getPrototypeOf(global);
    if (proto === Object.prototype || proto === null) Object.setPrototypeOf(global, Window.prototype);
    else { methods(global, { addEventListener: EventTarget.prototype.addEventListener, removeEventListener: EventTarget.prototype.removeEventListener, dispatchEvent: EventTarget.prototype.dispatchEvent }); def(global, Symbol.toStringTag, { value: 'Window', enumerable: false }); }
  })();
  if (typeof global.addEventListener !== 'function') methods(global, { addEventListener: EventTarget.prototype.addEventListener, removeEventListener: EventTarget.prototype.removeEventListener, dispatchEvent: EventTarget.prototype.dispatchEvent });
  // The prelude is strict code: a bare `addEventListener('load', f)` calls the prototype method with no
  // receiver (this === undefined) and threw. Window's own copies bind to the global object when called bare,
  // as in a browser where `addEventListener === window.addEventListener`.
  ['addEventListener', 'removeEventListener', 'dispatchEvent'].forEach(function (m) {
    var base = EventTarget.prototype[m];
    var f = function () { return base.apply(this === undefined || this === null ? global : this, arguments); };
    Object.defineProperty(f, 'name', { value: m, configurable: true });
    def(global, m, { value: f, enumerable: false });
  });

  // ---- viewport-derived values
  function viewport() { var v = null; try { v = sw.viewport(); } catch (e) { /* ignore */ } v = v || {}; return { width: +v.width || 1280, height: +v.height || 800, dsf: +v.dsf || 1, mobile: !!v.mobile, dark: !!v.dark }; }
  getters(global, {
    innerWidth: { get: function () { return viewport().width; }, set: function () { } }, innerHeight: { get: function () { return viewport().height; }, set: function () { } },
    outerWidth: { get: function () { return viewport().width; }, set: function () { } }, outerHeight: { get: function () { return viewport().height; }, set: function () { } },
    devicePixelRatio: { get: function () { return viewport().dsf; }, set: function () { } },
    scrollX: { get: function () { return scrollPos()[0]; } }, scrollY: { get: function () { return scrollPos()[1]; } }, pageXOffset: { get: function () { return scrollPos()[0]; } }, pageYOffset: { get: function () { return scrollPos()[1]; } },
    screenX: { get: function () { return 0; } }, screenY: { get: function () { return 0; } }, screenLeft: { get: function () { return 0; } }, screenTop: { get: function () { return 0; } }
  });
  global.scrollTo = global.scroll = scrollWinArgs(false);
  global.scrollBy = scrollWinArgs(true);
  global.moveTo = global.moveBy = global.resizeTo = global.resizeBy = global.focus = global.blur = global.print = global.stop = function () { };
  global.close = function () { };
  global.open = function (url) { if (url) sw.navigate(String(new global.URL(url, currentURL()))); return null; };
  global.getComputedStyle = function getComputedStyle(el) {
    if (!el || el.nodeType !== 1) throw new TypeError("Failed to execute 'getComputedStyle' on 'Window': parameter 1 is not of type 'Element'.");
    var text = sw.getAttr(el[ID], 'style') || '';
    var base = styleDecl(null, text);
    // a few sensible computed defaults when not set inline
    var defaults = { display: /^(div|p|h[1-6]|ul|ol|li|section|article|header|footer|nav|main|aside|form|table|body|html|pre|blockquote|hr|fieldset|details|summary|dialog|figure)$/.test(el.localName) ? 'block' : el.localName === 'script' || el.localName === 'style' || el.localName === 'head' || el.hasAttribute('hidden') ? 'none' : 'inline', visibility: 'visible', opacity: '1', position: 'static', 'pointer-events': 'auto', 'box-sizing': 'content-box', 'font-size': '16px', 'font-family': 'sans-serif', color: 'rgb(0, 0, 0)', 'background-color': 'rgba(0, 0, 0, 0)', width: hostBox(el[ID]).width + 'px', height: hostBox(el[ID]).height + 'px', direction: 'ltr', 'overflow-x': 'visible', 'overflow-y': 'visible', overflow: 'visible', 'z-index': 'auto', transform: 'none', 'text-align': 'start', 'line-height': 'normal', 'white-space': 'normal' };
    var proto = Object.getPrototypeOf(base);
    var computed = Object.create(proto);
    Object.defineProperty(computed, '_el', { value: null }); Object.defineProperty(computed, '_text', { value: text, writable: true });
    var origGPV = proto.getPropertyValue;
    // Values from the real cascade (computedstyle.go) come first; the inline style and the tag
    // defaults only answer properties the engine does not serialise yet.
    var real = null;
    try { real = typeof sw.computedStyle === 'function' ? sw.computedStyle(el[ID]) : null; } catch (e) { real = null; }
    Object.defineProperty(computed, 'getPropertyValue', { value: function (name) { var kn = String(name).toLowerCase(); if (real && real[kn] !== undefined && real[kn] !== null) return String(real[kn]); var v = origGPV.call(this, name); if (v === '') { name = String(name).toLowerCase(); if (defaults[name] !== undefined) return defaults[name]; if (/^(margin|padding|border|top|left|right|bottom|inset)/.test(name)) return name.indexOf('color') >= 0 ? 'rgb(0, 0, 0)' : name.indexOf('style') >= 0 ? 'none' : '0px'; } return v; } });
    Object.defineProperty(computed, 'setProperty', { value: function () { throw DOMException("Failed to execute 'setProperty' on 'CSSStyleDeclaration': These styles are computed, and therefore the 'display' property is read-only.", 'NoModificationAllowedError'); } });
    return new Proxy(computed, { get: function (t, k) { if (typeof k === 'string' && !(k in t) && /^[a-zA-Z-]+$/.test(k)) return t.getPropertyValue(k.indexOf('-') >= 0 ? k : kebab(k)); return t[k]; } });
  };
  global.matchMedia = function matchMedia(query) {
    query = String(query);
    var matches = false, m;
    if (host('mediaMatches')) matches = !!sw.mediaMatches(query); // the stylesheets' evaluator: CSS and script agree
    else if (/prefers-color-scheme\s*:\s*dark/.test(query)) matches = viewport().dark;
    else if (/prefers-color-scheme\s*:\s*light/.test(query)) matches = !viewport().dark;
    else if ((m = /\(\s*(min|max)-width\s*:\s*(\d+(?:\.\d+)?)px\s*\)/.exec(query))) matches = m[1] === 'min' ? viewport().width >= +m[2] : viewport().width <= +m[2];
    else if (/prefers-reduced-motion\s*:\s*reduce/.test(query)) matches = true;
    else if (/\(\s*hover\s*:\s*hover\s*\)/.test(query)) matches = !viewport().mobile;
    else if (/pointer\s*:\s*fine/.test(query)) matches = !viewport().mobile;
    else if (/pointer\s*:\s*coarse/.test(query)) matches = viewport().mobile;
    var mql = Object.create(EventTarget.prototype);
    mql.matches = matches; mql.media = query; mql.onchange = null;
    mql.addListener = function (fn) { if (fn) this.addEventListener('change', fn); };
    mql.removeListener = function (fn) { if (fn) this.removeEventListener('change', fn); };
    return mql;
  };
  global.MediaQueryList = function MediaQueryList() { illegal(); };

  // ---- Location
  var urlOverride = null; // set by history.pushState/replaceState (no reload)
  function currentURL() { if (urlOverride) return urlOverride; var u = ''; try { u = sw.url() || ''; } catch (e) { /* ignore */ } return u || 'about:blank'; }
  function originOf(u) { try { return new global.URL(u).origin; } catch (e) { return 'null'; } }
  // the origin of this document: the host knows about inherited (about:blank, srcdoc) and opaque (sandboxed, data:) origins
  function docOriginStr() { try { if (typeof sw.docOrigin === 'function') { var o = sw.docOrigin(); if (typeof o === 'string' && o) return o; } } catch (e) { /* fall through */ } return originOf(currentURL()); }
  function Location() { illegal(); }
  var LOC_PARTS = ['protocol', 'host', 'hostname', 'port', 'pathname', 'search', 'hash', 'origin'];
  LOC_PARTS.forEach(function (part) {
    def(Location.prototype, part, {
      get: function () { if (part === 'origin') return docOriginStr(); try { return new global.URL(currentURL())[part]; } catch (e) { return ''; } },
      set: function (v) { if (part === 'origin') return; var u = new global.URL(currentURL()); u[part] = v; if (part === 'hash') { var old = currentURL(); if (u.href === old) return; urlOverride = u.href; histRecord('push', u.href, null); historyState = null; fire(global, 'hashchange', { oldURL: old, newURL: u.href }); fire(global, 'popstate', { state: null }); scrollToFragment(); } else sw.navigate(u.href); },
      enumerable: true
    });
  });
  def(Location.prototype, 'href', { get: function () { return currentURL(); }, set: function (v) { global.location.assign(v); }, enumerable: true });
  def(Location.prototype, 'ancestorOrigins', { get: function () { return { length: 0, item: function () { return null; }, contains: function () { return false; } }; } });
  methods(Location.prototype, {
    assign: function (url) { var u = new global.URL(String(url), currentURL()).href; if (u.split('#')[0] === currentURL().split('#')[0] && u.indexOf('#') >= 0) { global.location.hash = u.slice(u.indexOf('#')); return; } sw.navigate(u); },
    replace: function (url) { sw.navigate(new global.URL(String(url), currentURL()).href); },
    reload: function () { sw.navigate(currentURL()); },
    toString: function () { return currentURL(); },
    valueOf: function () { return this; }
  });
  setTag(Location, 'Location'); global.Location = Location;
  var locationObj = Object.create(Location.prototype);
  def(global, 'location', { get: function () { return locationObj; }, set: function (v) { locationObj.assign(String(v)); }, enumerable: true });

  // ---- History
  // Session history of this document: pushState/replaceState/hash changes add or replace entries,
  // back/forward/go move between them (asynchronously, as in a browser) and fire popstate/hashchange.
  // Entries before the first or after the last one belong to other documents (Go owns those).
  var historyState = null, histEntries = [], histIdx = 0, histSeq = 0, histHooks = [];
  function hist() {
    if (!histEntries.length) histEntries.push({ url: currentURL(), state: null, key: 'k' + (++histSeq), id: 'i' + histSeq });
    return histEntries;
  }
  function histNotify(kind, entry, prev, info) { histHooks.slice().forEach(function (h) { try { h(kind, entry, prev, info); } catch (e) { /* observers must not break history */ } }); }
  function histRecord(kind, url, state) {
    var es = hist(), prev = es[histIdx];
    if (kind === 'push') {
      es.length = histIdx + 1;
      var e = { url: url, state: state, key: 'k' + (++histSeq), id: 'i' + histSeq };
      es.push(e); histIdx = es.length - 1;
      histNotify('push', e, prev);
    } else {
      prev.url = url; prev.state = state;
      histNotify('replace', prev, prev);
    }
  }
  function histTraverse(delta, then) {
    var es = hist(), target = histIdx + delta;
    if (target < 0 || target >= es.length || delta === 0) return false;
    setTimeoutImpl(function () {
      var cur = hist(), from = cur[histIdx], to = cur[target];
      if (!to || target >= cur.length) return;
      histIdx = target;
      historyState = to.state;
      urlOverride = to.url;
      if (host('setUrl')) { try { sw.setUrl(to.url); } catch (e) { /* ignore */ } }
      histNotify('traverse', to, from, { delta: delta });
      fire(global, 'popstate', { state: to.state });
      var fa = from.url.split('#'), ta = to.url.split('#');
      if (fa[0] === ta[0] && from.url !== to.url) fire(global, 'hashchange', { oldURL: from.url, newURL: to.url });
      if (then) then();
    }, [], 0, false);
    return true;
  }
  Object.defineProperty(global, '__swHistory', { value: {
    entries: function () { return hist(); }, index: function () { hist(); return histIdx; }, hooks: histHooks,
    record: histRecord, traverse: histTraverse, get state() { return historyState; }
  } });
  function History() { illegal(); }
  getters(History.prototype, { length: { get: function () { return hist().length; } }, state: { get: function () { return historyState; } }, scrollRestoration: { get: function () { return 'auto'; }, set: function () { } } });
  function historyUpdate(state, url, push) {
    var target = null;
    if (url !== undefined && url !== null) {
      var u = new global.URL(String(url), currentURL());
      if (u.origin !== originOf(currentURL())) throw DOMException("Failed to execute '" + (push ? 'pushState' : 'replaceState') + "' on 'History': A history state object with URL '" + u.href + "' cannot be created in a document with origin '" + originOf(currentURL()) + "'.", 'SecurityError');
      target = u.href;
    }
    state = state === undefined ? null : state;
    if (target === null) target = currentURL();
    historyState = state;
    if (target !== currentURL() || url !== undefined && url !== null) {
      urlOverride = target;
      if (host('setUrl')) { try { sw.setUrl(target); } catch (e) { /* ignore */ } }
      hostMutation(push ? 'pushState' : 'replaceState', 0);
    }
    histRecord(push ? 'push' : 'replace', target, state);
  }
  methods(History.prototype, {
    pushState: function (state, title, url) { historyUpdate(state, url, true); },
    replaceState: function (state, title, url) { historyUpdate(state, url, false); },
    back: function () { if (!histTraverse(-1)) fire(global, 'popstate', { state: null }); },
    forward: function () { if (!histTraverse(1)) fire(global, 'popstate', { state: null }); },
    go: function (n) { n = Math.trunc(+n) || 0; if (!n) { sw.navigate(currentURL()); return; } if (!histTraverse(n)) fire(global, 'popstate', { state: null }); }
  });
  setTag(History, 'History'); global.History = History;
  var historyObj = Object.create(History.prototype);
  def(global, 'history', { get: function () { return historyObj; }, enumerable: true });

  // ---- Navigator / Screen
  function Navigator() { illegal(); }
  function ua() { try { return sw.userAgent() || ''; } catch (e) { return ''; } }
  var clipboardText = '';
  getters(Navigator.prototype, {
    userAgent: { get: ua }, appVersion: { get: function () { return ua().replace(/^Mozilla\//, ''); } }, appName: { get: function () { return 'Netscape'; } }, appCodeName: { get: function () { return 'Mozilla'; } }, product: { get: function () { return 'Gecko'; } }, productSub: { get: function () { return '20030107'; } }, vendor: { get: function () { return ''; } }, vendorSub: { get: function () { return ''; } },
    language: { get: function () { return 'en-US'; } }, languages: { get: function () { return Object.freeze(['en-US', 'en']); } },
    platform: { get: function () { return viewport().mobile ? 'Linux armv8l' : 'Linux x86_64'; } },
    onLine: { get: function () { try { return sw.pwaOnline ? sw.pwaOnline() : true; } catch (e) { return true; } } }, cookieEnabled: { get: function () { return true; } }, doNotTrack: { get: function () { return null; } }, globalPrivacyControl: { get: function () { return false; } },
    hardwareConcurrency: { get: function () { return 1; } }, deviceMemory: { get: function () { return 1; } }, maxTouchPoints: { get: function () { return viewport().mobile ? 5 : 0; } },
    webdriver: { get: function () { return false; } }, pdfViewerEnabled: { get: function () { return false; } },
    plugins: { get: function () { var p = []; p.item = function () { return null; }; p.namedItem = function () { return null; }; p.refresh = function () { }; return p; } }, mimeTypes: { get: function () { var p = []; p.item = function () { return null; }; p.namedItem = function () { return null; }; return p; } },
    connection: { get: function () { return { effectiveType: '4g', downlink: 10, rtt: 50, saveData: false, addEventListener: function () { }, removeEventListener: function () { } }; } },
    permissions: { get: function () { return { query: function () { return Promise.resolve({ state: 'prompt', onchange: null, addEventListener: function () { }, removeEventListener: function () { } }); } }; } },
    serviceWorker: { get: function () { return undefined; } }, geolocation: { get: function () { return { getCurrentPosition: function (ok, err) { if (err) setTimeoutImpl(function () { err({ code: 1, message: 'User denied Geolocation', PERMISSION_DENIED: 1 }); }, [], 0, false); }, watchPosition: function () { return 0; }, clearWatch: function () { } }; } },
    mediaDevices: { get: function () { return undefined; } }, credentials: { get: function () { return undefined; } }, storage: { get: function () { return { estimate: function () { return Promise.resolve({ quota: 0, usage: 0 }); }, persist: function () { return Promise.resolve(false); } }; } },
    locks: { get: function () { return undefined; } }, wakeLock: { get: function () { return undefined; } }
  });
  methods(Navigator.prototype, {
    sendBeacon: function (url, data) { try { sw.httpAsync('POST', new global.URL(String(url), currentURL()).href, { 'content-type': 'text/plain;charset=UTF-8' }, data === undefined || data === null ? '' : typeof data === 'string' ? data : bodyToString(data), true); } catch (e) { return false; } return true; },
    javaEnabled: function () { return false; }, vibrate: function () { return false; }, share: function () { return Promise.reject(DOMException('Share not supported', 'NotAllowedError')); }, canShare: function () { return false; },
    registerProtocolHandler: function () { }, getBattery: function () { return Promise.resolve({ charging: true, level: 1, chargingTime: 0, dischargingTime: Infinity, addEventListener: function () { }, removeEventListener: function () { } }); },
    getGamepads: function () { return []; }, requestMediaKeySystemAccess: function () { return Promise.reject(new Error('unsupported')); }
  });
  setTag(Navigator, 'Navigator'); global.Navigator = Navigator;
  var navigatorObj = Object.create(Navigator.prototype);
  def(global, 'navigator', { get: function () { return navigatorObj; }, enumerable: true });
  global.clientInformation = navigatorObj;
  function Screen() { illegal(); }
  getters(Screen.prototype, { width: { get: function () { return viewport().width; } }, height: { get: function () { return viewport().height; } }, availWidth: { get: function () { return viewport().width; } }, availHeight: { get: function () { return viewport().height; } }, availLeft: { get: function () { return 0; } }, availTop: { get: function () { return 0; } }, colorDepth: { get: function () { return 24; } }, pixelDepth: { get: function () { return 24; } }, orientation: { get: function () { var v = viewport(); return { type: v.width >= v.height ? 'landscape-primary' : 'portrait-primary', angle: 0, addEventListener: function () { }, removeEventListener: function () { }, lock: function () { return Promise.resolve(); }, unlock: function () { } }; } }, isExtended: { get: function () { return false; } } });
  setTag(Screen, 'Screen'); global.Screen = Screen;
  def(global, 'screen', { value: Object.create(Screen.prototype), enumerable: true });
  global.visualViewport = { get width() { return viewport().width; }, get height() { return viewport().height; }, offsetLeft: 0, offsetTop: 0, pageLeft: 0, pageTop: 0, scale: 1, addEventListener: function () { }, removeEventListener: function () { }, onresize: null, onscroll: null };

  // ---- Storage
  function Storage() { illegal(); }
  // a document without an origin of its own (sandboxed frame, data: URL) gets a SecurityError from the host
  function storageHost(fn) {
    try { return fn(); } catch (e) {
      var m = e && typeof e.message === 'string' ? /^(SecurityError|QuotaExceededError): ([\s\S]*)$/.exec(e.message) : null;
      if (m) throw DOMException(m[2], m[1]);
      throw e;
    }
  }
  methods(Storage.prototype, {
    getItem: function (k) { var key = toStr(k); var v = storageHost(function () { return sw.storageGet(this._kind, key); }.bind(this)); return v === undefined || v === null ? null : String(v); },
    setItem: function (k, v) { var key = toStr(k), val = toStr(v); var old = this.getItem(key); var kind = this._kind; storageHost(function () { sw.storageSet(kind, key, val); }); storageEvent(this, key, old, val); },
    removeItem: function (k) { var key = toStr(k); var old = this.getItem(key); var kind = this._kind; storageHost(function () { sw.storageRemove(kind, key); }); if (old !== null) storageEvent(this, key, old, null); },
    clear: function () { var kind = this._kind; storageHost(function () { sw.storageClear(kind); }); storageEvent(this, null, null, null); },
    key: function (i) { var kind = this._kind; var keys = storageHost(function () { return sw.storageKeys(kind); }) || []; return i >= 0 && i < keys.length ? keys[i] : null; }
  });
  getter(Storage.prototype, 'length', function () { var kind = this._kind; return (storageHost(function () { return sw.storageKeys(kind); }) || []).length; });
  setTag(Storage, 'Storage'); global.Storage = Storage;
  function storageEvent(area, key, oldValue, newValue) { /* same-document writes do not fire 'storage' in browsers; kept as a hook */ }
  function makeStorage(kind) {
    var s = Object.create(Storage.prototype);
    Object.defineProperty(s, '_kind', { value: kind, configurable: true });
    return new Proxy(s, {
      get: function (t, k) { if (typeof k === 'string' && !(k in t)) { var v = t.getItem(k); return v === null ? undefined : v; } return t[k]; },
      set: function (t, k, v) { if (typeof k === 'string' && !(k in Storage.prototype)) { t.setItem(k, v); return true; } return false; },
      has: function (t, k) { return (typeof k === 'string' && t.getItem(k) !== null) || k in t; },
      deleteProperty: function (t, k) { if (typeof k === 'string') t.removeItem(k); return true; },
      ownKeys: function (t) { return (sw.storageKeys(kind) || []).slice(); },
      getOwnPropertyDescriptor: function (t, k) { if (typeof k !== 'string') return undefined; var v = t.getItem(k); return v === null ? undefined : { value: v, enumerable: true, configurable: true, writable: true }; }
    });
  }
  def(global, 'localStorage', { value: makeStorage('local'), enumerable: true });
  def(global, 'sessionStorage', { value: makeStorage('session'), enumerable: true });
  global.indexedDB = undefined; global.caches = undefined;

  // ---- console
  function describeNode(n) {
    if (n.nodeType === 1) { var s = '<' + n.localName; if (n.id) s += ' id="' + n.id + '"'; if (n.className) s += ' class="' + n.className + '"'; return s + '>'; }
    if (n.nodeType === 3) return '#text "' + String(n.data).slice(0, 40).replace(/\n/g, '\\n') + '"';
    if (n.nodeType === 8) return '<!--' + String(n.data).slice(0, 40) + '-->';
    if (n.nodeType === 9) return '#document';
    if (n.nodeType === 11) return '#document-fragment';
    return '#node';
  }
  function fmtValue(v, depth, seen) {
    depth = depth || 0;
    if (v === null) return 'null';
    if (v === undefined) return 'undefined';
    var t = typeof v;
    if (t === 'string') return depth === 0 ? v : JSON.stringify(v);
    if (t === 'number' || t === 'boolean' || t === 'bigint') return String(v) + (t === 'bigint' ? 'n' : '');
    if (t === 'symbol') return v.toString();
    if (t === 'function') return '[Function: ' + (v.name || 'anonymous') + ']';
    if (isNodeObj(v)) return describeNode(v);
    if (v instanceof Error) return v.stack ? String(v.stack) : v.name + ': ' + v.message;
    if (v instanceof Date) return isNaN(v.getTime()) ? 'Invalid Date' : v.toISOString();
    if (v instanceof RegExp) return v.toString();
    if (v instanceof Promise) return 'Promise {}';
    if (v instanceof Event) return v.constructor.name + ' {type: "' + v.type + '"}';
    if (v instanceof Map) return 'Map(' + v.size + ') ' + fmtValue(Array.from(v.entries()), depth + 1, seen);
    if (v instanceof Set) return 'Set(' + v.size + ') ' + fmtValue(Array.from(v), depth + 1, seen);
    if (ArrayBuffer.isView(v)) return v.constructor.name + '(' + v.length + ') [' + Array.prototype.slice.call(v, 0, 32).join(', ') + (v.length > 32 ? ', …' : '') + ']';
    if (v instanceof ArrayBuffer) return 'ArrayBuffer(' + v.byteLength + ')';
    if (depth > 6) return Array.isArray(v) ? '[Array]' : '[Object]';
    seen = seen || [];
    if (seen.indexOf(v) >= 0) return '[Circular]';
    seen.push(v);
    try {
      var parts, i;
      if (Array.isArray(v) || (v instanceof NodeList) || (v instanceof HTMLCollection)) {
        parts = [];
        var len = v.length;
        for (i = 0; i < Math.min(len, 100); i++) parts.push(fmtValue(v[i], depth + 1, seen));
        if (len > 100) parts.push('… ' + (len - 100) + ' more');
        return '[' + parts.join(', ') + ']';
      }
      if (typeof v.toJSON === 'function' && !(v instanceof Event)) { var j = v.toJSON(); if (j !== v) return fmtValue(j, depth + 1, seen); }
      parts = [];
      var keys = Object.keys(v);
      for (i = 0; i < Math.min(keys.length, 100); i++) parts.push((/^[A-Za-z_$][\w$]*$/.test(keys[i]) ? keys[i] : JSON.stringify(keys[i])) + ': ' + fmtValue(v[keys[i]], depth + 1, seen));
      if (keys.length > 100) parts.push('… ' + (keys.length - 100) + ' more');
      var ctor = v.constructor && v.constructor !== Object && v.constructor.name ? v.constructor.name + ' ' : '';
      return ctor + '{' + parts.join(', ') + '}';
    } finally { seen.pop(); }
  }
  function formatArgs(args) {
    var out = [], i = 0;
    if (typeof args[0] === 'string' && /%[sdifoOjc%]/.test(args[0])) {
      i = 1;
      out.push(args[0].replace(/%([sdifoOjc%])/g, function (m, c) {
        if (c === '%') return '%';
        if (i >= args.length) return m;
        var a = args[i++];
        switch (c) {
          case 's': return typeof a === 'string' ? a : fmtValue(a, 1);
          case 'd': case 'i': return String(parseInt(a, 10));
          case 'f': return String(parseFloat(a));
          case 'c': return '';
          case 'j': try { return JSON.stringify(a); } catch (e) { return '[Circular]'; }
          default: return fmtValue(a, 1);
        }
      }));
    }
    for (; i < args.length; i++) out.push(fmtValue(args[i], 0));
    return out.join(' ');
  }
  var consoleIndent = '', counts = {}, consoleTimers = {};
  function logAt(level) { return function () { hostLog(level, consoleIndent + formatArgs(slice.call(arguments)), callerLoc()); }; }
  var consoleObj = {
    log: logAt('log'), info: logAt('info'), warn: logAt('warn'), error: logAt('error'), debug: logAt('debug'),
    trace: function () { var e = new Error(); hostLog('log', consoleIndent + 'Trace: ' + formatArgs(slice.call(arguments)) + '\n' + String(e.stack || '')); },
    dir: function (v) { hostLog('log', consoleIndent + fmtValue(v, 1)); }, dirxml: logAt('log'),
    table: function (v) { hostLog('log', consoleIndent + fmtValue(v, 1)); },
    assert: function (cond) { if (!cond) { var rest = slice.call(arguments, 1); hostLog('error', consoleIndent + 'Assertion failed' + (rest.length ? ': ' + formatArgs(rest) : '')); } },
    group: function () { if (arguments.length) hostLog('log', consoleIndent + formatArgs(slice.call(arguments))); consoleIndent += '  '; }, groupEnd: function () { consoleIndent = consoleIndent.slice(2); },
    count: function (label) { label = label === undefined ? 'default' : String(label); counts[label] = (counts[label] || 0) + 1; hostLog('log', consoleIndent + label + ': ' + counts[label]); },
    countReset: function (label) { counts[label === undefined ? 'default' : String(label)] = 0; },
    time: function (label) { consoleTimers[label === undefined ? 'default' : String(label)] = nowMs(); },
    timeEnd: function (label) { label = label === undefined ? 'default' : String(label); if (consoleTimers[label] !== undefined) { hostLog('log', consoleIndent + label + ': ' + (nowMs() - consoleTimers[label]).toFixed(3) + ' ms'); delete consoleTimers[label]; } },
    timeLog: function (label) { label = label === undefined ? 'default' : String(label); if (consoleTimers[label] !== undefined) hostLog('log', consoleIndent + label + ': ' + (nowMs() - consoleTimers[label]).toFixed(3) + ' ms'); },
    timeStamp: function () { }, profile: function () { }, profileEnd: function () { }, clear: function () { }, context: function () { return consoleObj; }
  };
  consoleObj.groupCollapsed = consoleObj.group; consoleObj.exception = consoleObj.error;
  def(global, 'console', { value: consoleObj, enumerable: false });

  // ---- dialogs
  function dialog(kind, message, dflt) { var r = null; try { r = sw.dialog(kind, message === undefined ? '' : toStr(message), dflt === undefined ? '' : toStr(dflt)); } catch (e) { /* ignore */ } return r || { accepted: false, value: '' }; }
  global.alert = function alert(m) { dialog('alert', arguments.length ? m : ''); };
  global.confirm = function confirm(m) { return !!dialog('confirm', arguments.length ? m : '').accepted; };
  global.prompt = function prompt(m, d) { var r = dialog('prompt', arguments.length ? m : '', d === undefined ? '' : d); return r.accepted ? toStr(r.value) : null; };

  // ---- performance
  var perfMarks = [], perfMeasures = [];
  function Performance() { illegal(); }
  methods(Performance.prototype, {
    now: function () { return nowMs(); },
    mark: function (name, opts) { var e = { name: String(name), entryType: 'mark', startTime: opts && opts.startTime !== undefined ? opts.startTime : nowMs(), duration: 0, detail: opts && opts.detail !== undefined ? opts.detail : null, toJSON: function () { return { name: this.name, entryType: this.entryType, startTime: this.startTime, duration: this.duration }; } }; perfMarks.push(e); return e; },
    measure: function (name, start, end) {
      var s = 0, en = nowMs();
      if (start && typeof start === 'object') { s = start.start !== undefined ? (typeof start.start === 'string' ? markTime(start.start) : start.start) : 0; en = start.end !== undefined ? (typeof start.end === 'string' ? markTime(start.end) : start.end) : en; }
      else { if (start !== undefined) s = markTime(start); if (end !== undefined) en = markTime(end); }
      var e = { name: String(name), entryType: 'measure', startTime: s, duration: en - s, detail: null, toJSON: function () { return { name: this.name, entryType: this.entryType, startTime: this.startTime, duration: this.duration }; } };
      perfMeasures.push(e); return e;
    },
    clearMarks: function (name) { perfMarks = name === undefined ? [] : perfMarks.filter(function (m) { return m.name !== name; }); },
    clearMeasures: function (name) { perfMeasures = name === undefined ? [] : perfMeasures.filter(function (m) { return m.name !== name; }); },
    clearResourceTimings: function () { }, setResourceTimingBufferSize: function () { },
    getEntries: function () { return perfMarks.concat(perfMeasures); },
    getEntriesByType: function (type) { return type === 'mark' ? perfMarks.slice() : type === 'measure' ? perfMeasures.slice() : []; },
    getEntriesByName: function (name, type) { return this.getEntries().filter(function (e) { return e.name === name && (!type || e.entryType === type); }); },
    toJSON: function () { return { timeOrigin: this.timeOrigin }; }
  });
  // measure() also accepts the PerformanceTiming attribute names as marks (React's dev timings measure from
  // 'navigationStart'); they are all at the time origin here except the unload/redirect ones, which are 0 (unset)
  var TIMING_MARKS = /^(navigationStart|fetchStart|domainLookupStart|domainLookupEnd|connectStart|connectEnd|requestStart|responseStart|responseEnd|domLoading|domInteractive|domContentLoadedEventStart|domContentLoadedEventEnd|domComplete|loadEventStart|loadEventEnd)$/;
  function markTime(name) { for (var i = perfMarks.length - 1; i >= 0; i--) if (perfMarks[i].name === name) return perfMarks[i].startTime; if (TIMING_MARKS.test(name)) return 0; throw DOMException("Failed to execute 'measure' on 'Performance': The mark '" + name + "' does not exist.", 'SyntaxError'); }
  var perfObj = Object.create(Performance.prototype);
  var timeOrigin = Date.now();
  getters(perfObj, { timeOrigin: { get: function () { return timeOrigin; } }, timing: { get: function () { return { navigationStart: timeOrigin, fetchStart: timeOrigin, domainLookupStart: timeOrigin, domainLookupEnd: timeOrigin, connectStart: timeOrigin, connectEnd: timeOrigin, requestStart: timeOrigin, responseStart: timeOrigin, responseEnd: timeOrigin, domLoading: timeOrigin, domInteractive: timeOrigin, domContentLoadedEventStart: timeOrigin, domContentLoadedEventEnd: timeOrigin, domComplete: timeOrigin, loadEventStart: timeOrigin, loadEventEnd: timeOrigin, unloadEventStart: 0, unloadEventEnd: 0, redirectStart: 0, redirectEnd: 0, secureConnectionStart: 0 }; } }, navigation: { get: function () { return { type: 0, redirectCount: 0, TYPE_NAVIGATE: 0, TYPE_RELOAD: 1, TYPE_BACK_FORWARD: 2 }; } }, memory: { get: function () { return { jsHeapSizeLimit: 16777216, totalJSHeapSize: 4194304, usedJSHeapSize: 2097152 }; } }, eventCounts: { get: function () { return new Map(); } }, onresourcetimingbufferfull: { get: function () { return null; }, set: function () { } } });
  methods(perfObj, { addEventListener: function () { }, removeEventListener: function () { }, dispatchEvent: function () { return true; } });
  setTag(Performance, 'Performance'); global.Performance = Performance;
  def(global, 'performance', { value: perfObj, enumerable: true });

  // ---- crypto
  function Crypto() { illegal(); }
  var cryptoRand = function (n) {
    var b64 = sw.cryptoRandom(n);
    if (typeof Uint8Array.fromBase64 === 'function') return Uint8Array.fromBase64(b64);
    var s = atob(b64), u = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) u[i] = s.charCodeAt(i);
    return u;
  };
  methods(Crypto.prototype, {
    getRandomValues: function getRandomValues(arr) {
      if (arguments.length < 1) throw new TypeError("Failed to execute 'getRandomValues' on 'Crypto': 1 argument required, but only 0 present.");
      if (!ArrayBuffer.isView(arr) || arr instanceof DataView || arr instanceof Float32Array || arr instanceof Float64Array) throw DOMException("Failed to execute 'getRandomValues' on 'Crypto': The data argument must be an integer-type TypedArray", 'TypeMismatchError');
      if (arr.byteLength > 65536) throw DOMException("Failed to execute 'getRandomValues' on 'Crypto': The ArrayBufferView's byte length (" + arr.byteLength + ") exceeds the number of bytes of entropy available via this API (65536).", 'QuotaExceededError');
      new Uint8Array(arr.buffer, arr.byteOffset, arr.byteLength).set(cryptoRand(arr.byteLength));
      return arr;
    },
    randomUUID: function randomUUID() {
      var b = cryptoRand(16);
      b[6] = (b[6] & 0x0f) | 0x40; b[8] = (b[8] & 0x3f) | 0x80;
      var h = Array.prototype.map.call(b, function (x) { return (x < 16 ? '0' : '') + x.toString(16); }).join('');
      return h.slice(0, 8) + '-' + h.slice(8, 12) + '-' + h.slice(12, 16) + '-' + h.slice(16, 20) + '-' + h.slice(20);
    }
  });
  setTag(Crypto, 'Crypto'); global.Crypto = Crypto;
  var cryptoObj = Object.create(Crypto.prototype);
  def(global, 'crypto', { value: cryptoObj, enumerable: true });

  // ---- custom elements
  function CustomElementRegistry() { illegal(); }
  var ceWhenDefined = new Map();
  methods(CustomElementRegistry.prototype, {
    define: function (name, ctor, options) {
      name = String(name);
      if (!/^[a-z][.0-9_a-z]*-[-.0-9_a-z]*$/.test(name)) throw DOMException("Failed to execute 'define' on 'CustomElementRegistry': \"" + name + "\" is not a valid custom element name", 'SyntaxError');
      if (typeof ctor !== 'function') throw new TypeError("Failed to execute 'define' on 'CustomElementRegistry': parameter 2 is not of type 'Function'.");
      if (ceDefinitions.has(name)) throw DOMException("Failed to execute 'define' on 'CustomElementRegistry': the name \"" + name + "\" has already been used with this registry", 'NotSupportedError');
      ceDefinitions.forEach(function (d) { if (d.ctor === ctor) throw DOMException("Failed to execute 'define' on 'CustomElementRegistry': this constructor has already been used with this registry", 'NotSupportedError'); });
      var observed = [];
      try { observed = (ctor.observedAttributes || []).map(String); } catch (e) { /* ignore */ }
      ceDefinitions.set(name, { ctor: ctor, observed: observed, extends: options && options.extends });
      // upgrade existing elements (document order)
      var existing = [];
      try { existing = wrapAll(sw.elementsByTag(0, name)); } catch (e) { /* ignore */ }
      existing.forEach(function (el) { maybeUpgrade(el, name); });
      var w = ceWhenDefined.get(name);
      if (w) { w.resolve(ctor); ceWhenDefined.delete(name); }
    },
    get: function (name) { var d = ceDefinitions.get(String(name)); return d ? d.ctor : undefined; },
    getName: function (ctor) { var n = null; ceDefinitions.forEach(function (d, k) { if (d.ctor === ctor) n = k; }); return n; },
    whenDefined: function (name) {
      name = String(name);
      var d = ceDefinitions.get(name); if (d) return Promise.resolve(d.ctor);
      var w = ceWhenDefined.get(name);
      if (!w) { w = {}; w.promise = new Promise(function (res) { w.resolve = res; }); ceWhenDefined.set(name, w); }
      return w.promise;
    },
    upgrade: function (root) { if (root && root.nodeType === 1) { maybeUpgrade(root, root.localName); queryAll(root, '*').forEach(function (e) { maybeUpgrade(e, e.localName); }); } }
  });
  setTag(CustomElementRegistry, 'CustomElementRegistry'); global.CustomElementRegistry = CustomElementRegistry;
  def(global, 'customElements', { value: Object.create(CustomElementRegistry.prototype), enumerable: true });

  // ---- DOMParser / XMLSerializer
  function DOMParser() { if (!(this instanceof DOMParser)) throw new TypeError("Constructor DOMParser requires 'new'"); }
  methods(DOMParser.prototype, {
    parseFromString: function (str, type) {
      var d = createDetachedDocument();
      str = toStr(str);
      if (/^\s*<!doctype/i.test(str)) str = str.replace(/^\s*<!doctype[^>]*>/i, '');
      // setInnerHTML on a detached <html> element parses in body context on most parsers; place head/body explicitly when present
      var hasHtml = /<html[\s>]/i.test(str);
      if (hasHtml) {
        var tmp = documentWrapper.createElement('div');
        tmp.innerHTML = str;
        var root = d._detachedRoot;
        var parsed = queryOne(tmp, 'html') || tmp;
        var head = queryOne(parsed, 'head'), body = queryOne(parsed, 'body');
        if (head) d.head.replaceWith(head); if (body) d.body.replaceWith(body);
        if (!head && !body) { while (parsed.firstChild) d.body.appendChild(parsed.firstChild); }
        void root;
      } else {
        var m = /^\s*<head[\s>][\s\S]*?<\/head>/i.exec(str);
        if (m) { d.head.innerHTML = m[0].replace(/^\s*<head[^>]*>/i, '').replace(/<\/head>\s*$/i, ''); str = str.slice(m[0].length); }
        var bm = /^\s*<body[^>]*>([\s\S]*)<\/body>\s*$/i.exec(str);
        d.body.innerHTML = bm ? bm[1] : str;
      }
      Object.defineProperty(d, 'contentType', { value: String(type || 'text/html') });
      return d;
    }
  });
  setTag(DOMParser, 'DOMParser'); global.DOMParser = DOMParser;
  function XMLSerializer() { }
  methods(XMLSerializer.prototype, { serializeToString: function (n) { return n.nodeType === 9 ? (n.documentElement ? n.documentElement.outerHTML : '') : n.nodeType === 11 ? n._kids.map(function (k) { return k.nodeType === 1 ? k.outerHTML : k.nodeType === 3 ? k.data : ''; }).join('') : n.outerHTML !== undefined ? n.outerHTML : n.data; } });
  global.XMLSerializer = XMLSerializer;

  // ---- Image / Option / Audio constructors
  global.Image = function Image(w, h) { var img = documentWrapper.createElement('img'); if (w !== undefined) img.setAttribute('width', String(w | 0)); if (h !== undefined) img.setAttribute('height', String(h | 0)); return img; };
  global.Image.prototype = global.HTMLImageElement.prototype;
  global.Option = function Option(text, value, defaultSelected, selected) { var o = documentWrapper.createElement('option'); if (text !== undefined) o.textContent = toStr(text); if (value !== undefined) o.setAttribute('value', toStr(value)); if (defaultSelected) o.setAttribute('selected', ''); if (selected) o.selected = true; return o; };
  global.Option.prototype = global.HTMLOptionElement.prototype;
  global.Audio = function Audio(src) { var a = documentWrapper.createElement('audio'); a.setAttribute('preload', 'auto'); if (src !== undefined) a.setAttribute('src', toStr(src)); return a; };
  global.Audio.prototype = global.HTMLAudioElement.prototype;

  // ---- Intl: QuickJS ships none. The Intl object exists here; its members (NumberFormat, DateTimeFormat, ...) load
  // lazily from platformjs/intl*.js (boot.js). The toLocale* methods below go through them with a small cache.
  if (typeof global.Intl === 'undefined') {
    var IntlObj = {};
    Object.defineProperty(IntlObj, Symbol.toStringTag, { value: 'Intl', configurable: true });
    def(global, 'Intl', { value: IntlObj, writable: true, configurable: true, enumerable: false });
  }
  var fmtCache = new Map();
  function cachedFormat(name, locales, options) {
    var key;
    try { key = name + '|' + (Array.isArray(locales) ? locales.join(',') : String(locales)) + '|' + (options === undefined ? '' : JSON.stringify(options)); } catch (e) { return new global.Intl[name](locales, options); }
    var f = fmtCache.get(key);
    if (!f) { f = new global.Intl[name](locales, options); if (fmtCache.size >= 64) fmtCache.clear(); fmtCache.set(key, f); }
    return f;
  }
  function method(proto, name, fn) { Object.defineProperty(proto, name, { value: fn, writable: true, configurable: true, enumerable: false }); Object.defineProperty(fn, 'name', { value: name, configurable: true }); }
  method(Number.prototype, 'toLocaleString', function (locales, options) { return cachedFormat('NumberFormat', locales, options).format(Number.prototype.valueOf.call(this)); });
  if (typeof BigInt === 'function') method(BigInt.prototype, 'toLocaleString', function (locales, options) { return cachedFormat('NumberFormat', locales, options).format(BigInt.prototype.valueOf.call(this)); });
  // ToDateTimeOptions (ECMA-402): which fields a toLocale*String call adds when the options name none
  function dateOptions(options, required, defaults) {
    if (options === undefined) options = {}; else if (options === null) throw new TypeError('Cannot convert undefined or null to object'); else options = Object.assign({}, Object(options));
    var need = true, k;
    if (required === 'date' || required === 'any') for (k of ['weekday', 'year', 'month', 'day']) if (options[k] !== undefined) need = false;
    if (required === 'time' || required === 'any') for (k of ['dayPeriod', 'hour', 'minute', 'second', 'fractionalSecondDigits']) if (options[k] !== undefined) need = false;
    if (options.dateStyle !== undefined || options.timeStyle !== undefined) need = false;
    if (required === 'date' && options.timeStyle !== undefined) throw new TypeError('Invalid option : timeStyle');
    if (required === 'time' && options.dateStyle !== undefined) throw new TypeError('Invalid option : dateStyle');
    if (need && (defaults === 'date' || defaults === 'all')) { options.year = 'numeric'; options.month = 'numeric'; options.day = 'numeric'; }
    if (need && (defaults === 'time' || defaults === 'all')) { options.hour = 'numeric'; options.minute = 'numeric'; options.second = 'numeric'; }
    return options;
  }
  function dateLocale(self, locales, options, required, defaults) {
    var t = Date.prototype.getTime.call(self);
    if (t !== t) return 'Invalid Date';
    return new global.Intl.DateTimeFormat(locales, dateOptions(options, required, defaults)).format(t);
  }
  method(Date.prototype, 'toLocaleString', function (locales, options) { return dateLocale(this, locales, options, 'any', 'all'); });
  method(Date.prototype, 'toLocaleDateString', function (locales, options) { return dateLocale(this, locales, options, 'date', 'date'); });
  method(Date.prototype, 'toLocaleTimeString', function (locales, options) { return dateLocale(this, locales, options, 'time', 'time'); });
  method(String.prototype, 'localeCompare', function (that, locales, options) {
    if (this === null || this === undefined) throw new TypeError('String.prototype.localeCompare called on null or undefined');
    return cachedFormat('Collator', locales, options).compare(String(this), String(that));
  });
  method(Array.prototype, 'toLocaleString', function (locales, options) {
    var out = [];
    for (var i = 0; i < this.length; i++) { var x = this[i]; out.push(x === null || x === undefined ? '' : String(x.toLocaleString(locales, options))); }
    return out.join(',');
  });

  // ---- WebSocket / Worker / Notification / misc stubs
  function WebSocket(url) {
    if (!(this instanceof WebSocket)) throw new TypeError("Constructor WebSocket requires 'new'");
    EventTarget.call(this);
    this.url = String(url); this.readyState = 3; this.protocol = ''; this.extensions = ''; this.bufferedAmount = 0; this.binaryType = 'blob';
    var self = this;
    setTimeoutImpl(function () { fire(self, 'error', { bubbles: false, cancelable: false }); dispatch(self, new global.CloseEvent('close', { wasClean: false, code: 1006, reason: 'WebSocket is not available in Sweef' })); }, [], 0, false);
  }
  extend(WebSocket, EventTarget);
  methods(WebSocket.prototype, { send: function () { throw DOMException("Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.", 'InvalidStateError'); }, close: function () { } });
  defineHandlerProps(WebSocket.prototype, ['open', 'message', 'error', 'close']);
  constant(WebSocket, 'CONNECTING', 0); constant(WebSocket, 'OPEN', 1); constant(WebSocket, 'CLOSING', 2); constant(WebSocket, 'CLOSED', 3);
  constant(WebSocket.prototype, 'CONNECTING', 0); constant(WebSocket.prototype, 'OPEN', 1); constant(WebSocket.prototype, 'CLOSING', 2); constant(WebSocket.prototype, 'CLOSED', 3);
  setTag(WebSocket, 'WebSocket'); global.WebSocket = WebSocket;
  global.EventSource = function EventSource(url) { EventTarget.call(this); this.url = String(url); this.readyState = 2; var self = this; setTimeoutImpl(function () { fire(self, 'error', {}); }, [], 0, false); };
  extend(global.EventSource, EventTarget); methods(global.EventSource.prototype, { close: function () { } }); defineHandlerProps(global.EventSource.prototype, ['open', 'message', 'error']);
  // Notification: platformjs/pwa.js
  global.speechSynthesis = { speak: function () { }, cancel: function () { }, pause: function () { }, resume: function () { }, getVoices: function () { return []; }, speaking: false, pending: false, paused: false, addEventListener: function () { }, removeEventListener: function () { }, onvoiceschanged: null };
  global.SpeechSynthesisUtterance = function SpeechSynthesisUtterance(text) { this.text = text; };
  global.CSS = { supports: function () { return false; }, escape: function (s) { return String(s).replace(/([\0-\x1f\x7f]|^-?\d)|^-$|[^\x80-￿\w-]/g, function (ch, asCode) { return asCode ? (ch === '\0' ? '�' : ch.slice(0, -1) + '\\' + ch.slice(-1).charCodeAt(0).toString(16) + ' ') : '\\' + ch; }); }, registerProperty: function () { }, px: function (v) { return v + 'px'; }, highlights: new Map() };
  global.CSSStyleSheet = function CSSStyleSheet() { this.cssRules = []; this.rules = this.cssRules; this.disabled = false; this.href = null; this.media = { mediaText: '' }; this.ownerNode = null; this.title = null; this.type = 'text/css'; };
  // ruleBody is the declaration block of a rule text: everything between the first '{' and the last '}'.
  function ruleBody(r) { var t = String(r), b = t.indexOf('{'), e = t.lastIndexOf('}'); return b < 0 ? '' : t.slice(b + 1, e > b ? e : t.length); }
  global.CSSStyleSheet.prototype = { insertRule: function (r, i) { this.cssRules.splice(i || 0, 0, { cssText: String(r), type: 1, selectorText: String(r).split('{')[0].trim(), style: styleDecl(null, ruleBody(r)) }); return i || 0; }, deleteRule: function (i) { this.cssRules.splice(i, 1); }, addRule: function (sel, style, i) { return this.insertRule(sel + ' {' + style + '}', i === undefined ? this.cssRules.length : i); }, removeRule: function (i) { this.deleteRule(i); }, replace: function (text) { this.replaceSync(text); return Promise.resolve(this); }, replaceSync: function (text) { this.cssRules.length = 0; var self = this; String(text).replace(/[^{}]+\{[^{}]*\}/g, function (rule) { self.insertRule(rule.trim(), self.cssRules.length); }); } };
  getters(Document.prototype, { adoptedStyleSheets: { get: function () { return slots(this).adopted || (slots(this).adopted = []); }, set: function (v) { slots(this).adopted = Array.from(v); } } });
  global.ResizeObserverEntry = function ResizeObserverEntry() { illegal(); };
  global.FontFace = function FontFace(family, source, desc) { this.family = family; this.status = 'loaded'; this.loaded = Promise.resolve(this); Object.assign(this, desc || {}); };
  global.FontFace.prototype.load = function () { return Promise.resolve(this); };
  global.ImageData = function ImageData(w, h) { if (typeof w === 'object') { this.data = w; this.width = h; this.height = arguments[2]; } else { this.width = w; this.height = h; this.data = new Uint8ClampedArray(w * h * 4); } };
  global.Path2D = function Path2D() { }; ['moveTo', 'lineTo', 'closePath', 'rect', 'arc', 'arcTo', 'bezierCurveTo', 'quadraticCurveTo', 'ellipse', 'addPath'].forEach(function (m) { global.Path2D.prototype[m] = function () { }; });
  global.OffscreenCanvas = function OffscreenCanvas(w, h) { this.width = w; this.height = h; }; global.OffscreenCanvas.prototype.getContext = function () { return null; }; global.OffscreenCanvas.prototype.convertToBlob = function () { return Promise.resolve(new global.Blob([])); };
  global.DOMMatrix = function DOMMatrix() { this.a = 1; this.b = 0; this.c = 0; this.d = 1; this.e = 0; this.f = 0; this.m11 = 1; this.m22 = 1; this.m33 = 1; this.m44 = 1; this.is2D = true; this.isIdentity = true; }; global.DOMMatrix.prototype = { translate: function () { return this; }, scale: function () { return this; }, rotate: function () { return this; }, multiply: function () { return this; }, inverse: function () { return this; }, transformPoint: function (p) { return p || { x: 0, y: 0, z: 0, w: 1 }; }, toString: function () { return 'matrix(1, 0, 0, 1, 0, 0)'; } }; global.DOMMatrixReadOnly = global.DOMMatrix; global.WebKitCSSMatrix = global.DOMMatrix;
  global.DOMPoint = function DOMPoint(x, y, z, w) { this.x = +x || 0; this.y = +y || 0; this.z = +z || 0; this.w = w === undefined ? 1 : +w; }; global.DOMPoint.prototype.matrixTransform = function () { return this; }; global.DOMPointReadOnly = global.DOMPoint;
  global.reportError = function (e) { reportError(e); };
  global.structuredClone = function structuredClone(v) { return clone(v, new Map()); };
  function clone(v, seen) {
    if (v === null || typeof v !== 'object') { if (typeof v === 'function' || typeof v === 'symbol') throw DOMException((typeof v === 'function' ? v.toString().slice(0, 60) : String(v)) + ' could not be cloned.', 'DataCloneError'); return v; }
    if (seen.has(v)) return seen.get(v);
    if (isNodeObj(v)) throw DOMException('HTMLElement object could not be cloned.', 'DataCloneError');
    var out;
    if (v instanceof Date) return new Date(v.getTime());
    if (v instanceof RegExp) return new RegExp(v.source, v.flags);
    if (v instanceof ArrayBuffer) return v.slice(0);
    if (v._isVF || v._isAD) return v.clone(); // WebCodecs frames
    if (global.__swSAB && v instanceof global.__swSAB) return typeof global.__swSabShare === 'function' ? global.__swSabShare(v) : v;
    if (ArrayBuffer.isView(v)) return v.buffer && global.__swSAB && v.buffer instanceof global.__swSAB ? new v.constructor(clone(v.buffer, seen), v.byteOffset, v instanceof DataView ? v.byteLength : v.length) : new v.constructor(v);
    if (v instanceof Map) { out = new Map(); seen.set(v, out); v.forEach(function (val, k) { out.set(clone(k, seen), clone(val, seen)); }); return out; }
    if (v instanceof Set) { out = new Set(); seen.set(v, out); v.forEach(function (val) { out.add(clone(val, seen)); }); return out; }
    if (v instanceof Error) { out = new (global[v.name] || Error)(v.message); out.stack = v.stack; return out; }
    if (Array.isArray(v)) { out = new Array(v.length); seen.set(v, out); for (var i = 0; i < v.length; i++) if (i in v) out[i] = clone(v[i], seen); return out; }
    if (v instanceof global.Blob) return v;
    out = {}; seen.set(v, out);
    Object.keys(v).forEach(function (k) { out[k] = clone(v[k], seen); });
    return out;
  }

  // ---- base64 / text encoding
  var B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
  global.btoa = function btoa(s) {
    if (arguments.length < 1) throw new TypeError("Failed to execute 'btoa' on 'Window': 1 argument required, but only 0 present.");
    var r = sw.b64enc(toStr(s));
    if (r === null) throw DOMException("Failed to execute 'btoa' on 'Window': The string to be encoded contains characters outside of the Latin1 range.", 'InvalidCharacterError');
    return r;
  };
  global.atob = function atob(s) {
    if (arguments.length < 1) throw new TypeError("Failed to execute 'atob' on 'Window': 1 argument required, but only 0 present.");
    var r = sw.b64dec(toStr(s));
    if (r === null) throw DOMException("Failed to execute 'atob' on 'Window': The string to be decoded is not correctly encoded.", 'InvalidCharacterError');
    return r;
  };
  var B64LUT = null;
  function b64ToBytes(str) {
    var bin = sw.b64dec(toStr(str)); // Go decodes; the loop only copies the bytes into the typed array
    if (bin !== null) {
      var bn = bin.length, bo = new Uint8Array(bn), bi = 0;
      for (; bi + 4 <= bn; bi += 4) { bo[bi] = bin.charCodeAt(bi); bo[bi + 1] = bin.charCodeAt(bi + 1); bo[bi + 2] = bin.charCodeAt(bi + 2); bo[bi + 3] = bin.charCodeAt(bi + 3); }
      for (; bi < bn; bi++) bo[bi] = bin.charCodeAt(bi);
      return bo;
    }
    if (!B64LUT) { B64LUT = new Uint8Array(256).fill(255); for (var k = 0; k < 64; k++) B64LUT[B64.charCodeAt(k)] = k; }
    var n = str.length, pad = n && str.charCodeAt(n - 1) === 61 ? (str.charCodeAt(n - 2) === 61 ? 2 : 1) : 0;
    var out = new Uint8Array((n * 3 >> 2) - pad), o = 0, buf = 0, bits = 0;
    for (var i = 0; i < n; i++) { var v = B64LUT[str.charCodeAt(i)]; if (v === 255) continue; buf = (buf << 6) | v; bits += 6; if (bits >= 8) { bits -= 8; if (o < out.length) out[o++] = (buf >> bits) & 255; buf &= (1 << bits) - 1; } }
    return out;
  }
  function utf8Encode(str) {
    str = toStr(str); var out = [];
    if (global.__swUtf8Encode) return new Uint8Array(global.__swUtf8Encode(str)); // the V8 runtime encodes natively
    for (var i = 0; i < str.length; i++) {
      var c = str.charCodeAt(i);
      if (c >= 0xd800 && c < 0xdc00 && i + 1 < str.length) { var d = str.charCodeAt(i + 1); if (d >= 0xdc00 && d < 0xe000) { c = 0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00); i++; } else c = 0xfffd; } else if (c >= 0xd800 && c < 0xe000) c = 0xfffd;
      if (c < 0x80) out.push(c);
      else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
      else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
      else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
    }
    return new Uint8Array(out);
  }
  function utf8Decode(bytes, fatal) {
    if (!fatal && global.__swUtf8Decode) { var nat = global.__swUtf8Decode(bytes); if (nat !== undefined) return nat; } // the V8 runtime decodes natively
    var out = '', i = 0, n = bytes.length;
    while (i < n) {
      var b = bytes[i++], c;
      if (b < 0x80) c = b;
      else if (b >= 0xc2 && b < 0xe0 && i < n) c = ((b & 31) << 6) | (bytes[i++] & 63);
      else if (b >= 0xe0 && b < 0xf0 && i + 1 < n) { c = ((b & 15) << 12) | ((bytes[i] & 63) << 6) | (bytes[i + 1] & 63); i += 2; }
      else if (b >= 0xf0 && b < 0xf5 && i + 2 < n) { c = ((b & 7) << 18) | ((bytes[i] & 63) << 12) | ((bytes[i + 1] & 63) << 6) | (bytes[i + 2] & 63); i += 3; }
      else { if (fatal) throw new TypeError('The encoded data was not valid.'); c = 0xfffd; }
      if (c >= 0x10000) { c -= 0x10000; out += String.fromCharCode(0xd800 + (c >> 10), 0xdc00 + (c & 1023)); } else out += String.fromCharCode(c);
    }
    return out;
  }
  function toBytes(v) {
    if (v instanceof ArrayBuffer) return new Uint8Array(v);
    if (ArrayBuffer.isView(v)) return new Uint8Array(v.buffer, v.byteOffset, v.byteLength);
    if (typeof v === 'string') return utf8Encode(v);
    throw new TypeError('expected an ArrayBuffer or ArrayBufferView');
  }
  function TextEncoder() { if (!(this instanceof TextEncoder)) throw new TypeError("Constructor TextEncoder requires 'new'"); }
  TextEncoder.prototype = { constructor: TextEncoder, get encoding() { return 'utf-8'; }, encode: function (s) { return utf8Encode(s === undefined ? '' : s); }, encodeInto: function (s, dest) { var b = utf8Encode(s); var n = Math.min(b.length, dest.length); dest.set(b.subarray(0, n)); return { read: s.length, written: n }; } };
  setTag(TextEncoder, 'TextEncoder'); global.TextEncoder = TextEncoder;
  function TextDecoder(label, opts) { if (!(this instanceof TextDecoder)) throw new TypeError("Constructor TextDecoder requires 'new'"); this._label = String(label || 'utf-8').toLowerCase(); this.fatal = !!(opts && opts.fatal); this.ignoreBOM = !!(opts && opts.ignoreBOM); }
  TextDecoder.prototype = { constructor: TextDecoder, get encoding() { return this._label === 'utf8' ? 'utf-8' : this._label; }, decode: function (v) { if (v === undefined) return ''; var b = toBytes(v); if (/latin1|iso-8859-1|ascii|windows-1252/.test(this._label)) { var s = ''; for (var i = 0; i < b.length; i++) s += String.fromCharCode(b[i]); return s; } if (!this.ignoreBOM && b.length >= 3 && b[0] === 0xef && b[1] === 0xbb && b[2] === 0xbf) b = b.subarray(3); return utf8Decode(b, this.fatal); } };
  setTag(TextDecoder, 'TextDecoder'); global.TextDecoder = TextDecoder;

  // ---- URL / URLSearchParams
  var DEFAULT_PORTS = { 'http:': '80', 'https:': '443', 'ws:': '80', 'wss:': '443', 'ftp:': '21' };
  var SPECIAL = { 'http:': 1, 'https:': 1, 'ws:': 1, 'wss:': 1, 'ftp:': 1, 'file:': 1 };
  function urlError(u) { return new TypeError("Failed to construct 'URL': Invalid URL" + (u !== undefined ? ": " + u : '')); }
  function parseURL(input, base) {
    input = String(input).trim().replace(/[\t\n\r]/g, '');
    var m = /^([a-zA-Z][a-zA-Z0-9+.-]*):(.*)$/.exec(input);
    if (m && base !== undefined && SPECIAL[m[1].toLowerCase() + ':'] && !/^[\/\\]{2}/.test(m[2])) {
      // "https:../x" against an https base is a relative reference (special relative or authority state)
      var sb = null; try { sb = parseURL(base); } catch (e) { /* not a valid base */ }
      if (sb && sb.protocol === m[1].toLowerCase() + ':') { input = m[2]; m = null; }
    }
    var rec = { protocol: '', username: '', password: '', hostname: '', port: '', pathname: '', search: '', hash: '', opaque: false };
    var rest;
    if (m) { rec.protocol = m[1].toLowerCase() + ':'; rest = m[2]; }
    else {
      if (!base) throw urlError(input);
      var b = parseURL(base);
      rec.protocol = b.protocol;
      if (b.opaque && !/^[#?]/.test(input)) throw urlError(input);
      if (input.indexOf('//') === 0) { rest = input; }
      else {
        rec.username = b.username; rec.password = b.password; rec.hostname = b.hostname; rec.port = b.port;
        var hashI = input.indexOf('#'); if (hashI >= 0) { rec.hash = input.slice(hashI); input = input.slice(0, hashI); }
        var qI = input.indexOf('?'); if (qI >= 0) { rec.search = input.slice(qI); input = input.slice(0, qI); }
        if (input === '') { rec.pathname = b.pathname; if (qI < 0) rec.search = b.search; }
        else if (input[0] === '/') rec.pathname = normalizePath(input);
        else rec.pathname = normalizePath(b.pathname.replace(/[^\/]*$/, '') + input);
        if (b.opaque) { rec.opaque = true; rec.pathname = b.pathname; }
        if (rec.hash && !/^#/.test(rec.hash)) rec.hash = '#' + rec.hash;
        return rec;
      }
    }
    var special = !!SPECIAL[rec.protocol];
    var hi = rest.indexOf('#'); if (hi >= 0) { rec.hash = rest.slice(hi); rest = rest.slice(0, hi); if (rec.hash === '#') rec.hash = ''; }
    var qi = rest.indexOf('?'); if (qi >= 0) { rec.search = rest.slice(qi); rest = rest.slice(0, qi); if (rec.search === '?') rec.search = ''; }
    if (special) rest = rest.replace(/\\/g, '/');
    if (rest.indexOf('//') === 0 || (special && rec.protocol !== 'file:')) {
      rest = rest.replace(/^\/*/, '');
      var slash = rest.indexOf('/');
      var auth = slash >= 0 ? rest.slice(0, slash) : rest;
      rest = slash >= 0 ? rest.slice(slash) : '';
      var at = auth.lastIndexOf('@');
      if (at >= 0) { var cred = auth.slice(0, at); auth = auth.slice(at + 1); var ci = cred.indexOf(':'); rec.username = ci >= 0 ? cred.slice(0, ci) : cred; rec.password = ci >= 0 ? cred.slice(ci + 1) : ''; }
      var pm = /^(\[[^\]]*\]|[^:]*)(?::(\d*))?$/.exec(auth);
      if (!pm) throw urlError(input);
      rec.hostname = pm[1].toLowerCase(); rec.port = pm[2] || '';
      if (rec.port === DEFAULT_PORTS[rec.protocol]) rec.port = '';
      if (rec.port && !/^\d+$/.test(rec.port)) throw urlError(input);
      if (special && rec.protocol !== 'file:' && !rec.hostname) throw urlError(input);
      if (/[\s<>^`{|}]/.test(rec.hostname)) throw urlError(input);
      rec.pathname = special ? normalizePath(rest || '/') : rest;
    } else if (rec.protocol === 'file:') { rec.pathname = normalizePath(rest || '/'); }
    else { rec.opaque = rest[0] !== '/'; rec.pathname = rest; }
    return rec;
  }
  function normalizePath(p) {
    var segs = p.split('/'), out = [];
    for (var i = 0; i < segs.length; i++) {
      var s = segs[i];
      if (s === '..' || s === '%2e%2e' || s === '.%2e' || s === '%2e.') { if (out.length > 1) out.pop(); if (i === segs.length - 1) out.push(''); }
      else if (s === '.' || s === '%2e') { if (i === segs.length - 1) out.push(''); }
      else out.push(i === 0 ? s : encodePath(s));
    }
    var r = out.join('/');
    return r[0] === '/' ? r : '/' + r;
  }
  function encodePath(s) { return s.replace(/[\s"<>`{}^|\\\u0000-\u001f\u007f-￿]/g, function (c) { return c === ' ' ? '%20' : encodeURIComponent(c); }); }
  function encodeQuery(s) { return s.replace(/[\s"<>`#\u0000-\u001f\u007f-￿']/g, function (c) { return c === ' ' ? '%20' : c === "'" ? '%27' : encodeURIComponent(c); }); }
  function encodeFragment(s) { return s.replace(/[\s"<>`\u0000-\u001f\u007f-￿]/g, function (c) { return c === ' ' ? '%20' : encodeURIComponent(c); }); }
  function URL(url, base) {
    if (!(this instanceof URL)) throw new TypeError("Failed to construct 'URL': Please use the 'new' operator");
    if (arguments.length === 0) throw new TypeError("Failed to construct 'URL': 1 argument required, but only 0 present.");
    var rec = parseURL(url, base === undefined ? undefined : String(base));
    Object.defineProperty(this, '_r', { value: rec, writable: true });
    var self = this;
    Object.defineProperty(this, 'searchParams', { value: new URLSearchParams(rec.search, function (s) { self._r.search = s ? '?' + s : ''; }), enumerable: true });
  }
  function serializeURL(r, omitHash) {
    var s = r.protocol;
    if (r.hostname || SPECIAL[r.protocol] && r.protocol !== 'file:' || (r.protocol === 'file:')) { s += '//'; if (r.username || r.password) s += r.username + (r.password ? ':' + r.password : '') + '@'; s += r.hostname; if (r.port) s += ':' + r.port; }
    else if (!r.opaque && r.pathname.indexOf('//') === 0) s += '/.';
    s += r.pathname + r.search;
    if (!omitHash) s += r.hash;
    return s;
  }
  URL.prototype = {
    constructor: URL,
    get href() { return serializeURL(this._r); }, set href(v) { this._r = parseURL(v); this.searchParams._parse(this._r.search); },
    get origin() { var r = this._r; if (r.protocol === 'blob:') { try { return new URL(r.pathname).origin; } catch (e) { return 'null'; } } return SPECIAL[r.protocol] && r.protocol !== 'file:' ? r.protocol + '//' + r.hostname + (r.port ? ':' + r.port : '') : 'null'; },
    get protocol() { return this._r.protocol; }, set protocol(v) { v = String(v).replace(/:.*$/, '').toLowerCase(); if (/^[a-z][a-z0-9+.-]*$/.test(v) && (!!SPECIAL[v + ':'] === !!SPECIAL[this._r.protocol])) { this._r.protocol = v + ':'; if (this._r.port === DEFAULT_PORTS[this._r.protocol]) this._r.port = ''; } },
    get username() { return this._r.username; }, set username(v) { if (this._r.hostname) this._r.username = encodeURIComponent(String(v)); },
    get password() { return this._r.password; }, set password(v) { if (this._r.hostname) this._r.password = encodeURIComponent(String(v)); },
    get host() { return this._r.hostname + (this._r.port ? ':' + this._r.port : ''); }, set host(v) { if (this._r.opaque) return; var m = /^([^:\/?#]*)(?::(\d*))?/.exec(String(v)); if (m && m[1]) { this._r.hostname = m[1].toLowerCase(); if (m[2] !== undefined) this.port = m[2]; } },
    get hostname() { return this._r.hostname; }, set hostname(v) { if (this._r.opaque) return; v = String(v).replace(/[\/?#:].*$/, '').toLowerCase(); if (v) this._r.hostname = v; },
    get port() { return this._r.port; }, set port(v) { if (this._r.opaque || !this._r.hostname || this._r.protocol === 'file:') return; v = String(v).replace(/\D.*$/, ''); if (v === '') { this._r.port = ''; return; } if (+v > 65535) return; this._r.port = String(+v) === DEFAULT_PORTS[this._r.protocol] ? '' : String(+v); },
    get pathname() { return this._r.pathname; }, set pathname(v) { if (this._r.opaque) return; v = String(v); if (SPECIAL[this._r.protocol]) v = v.replace(/\\/g, '/'); this._r.pathname = SPECIAL[this._r.protocol] ? normalizePath(v[0] === '/' ? v : '/' + v) : v; },
    get search() { return this._r.search; }, set search(v) { v = String(v); if (v && v[0] !== '?') v = '?' + v; this._r.search = v === '?' ? '' : encodeQuery(v); this.searchParams._parse(this._r.search); },
    get hash() { return this._r.hash; }, set hash(v) { v = String(v); if (v && v[0] !== '#') v = '#' + v; this._r.hash = v === '#' ? '' : encodeFragment(v); },
    toString: function () { return this.href; }, toJSON: function () { return this.href; }
  };
  URL.canParse = function (u, b) { try { parseURL(u, b === undefined ? undefined : String(b)); return true; } catch (e) { return false; } };
  URL.parse = function (u, b) { try { return new URL(u, b); } catch (e) { return null; } };
  var blobURLs = new Map(), blobSeq = 0;
  URL.createObjectURL = function (blob) { var u = 'blob:' + docOriginStr() + '/' + (++blobSeq) + '-' + cryptoObj.randomUUID(); blobURLs.set(u, blob); return u; };
  URL.revokeObjectURL = function (u) { blobURLs.delete(String(u)); };
  setTag(URL, 'URL'); global.URL = URL; global.webkitURL = URL;

  function formEncode(s) { return encodeURIComponent(s).replace(/%20/g, '+').replace(/[!'()~]/g, function (c) { return '%' + c.charCodeAt(0).toString(16).toUpperCase(); }); }
  function formDecode(s) { try { return decodeURIComponent(s.replace(/\+/g, ' ')); } catch (e) { return s; } }
  function URLSearchParams(init, onChange) {
    if (!(this instanceof URLSearchParams)) throw new TypeError("Failed to construct 'URLSearchParams': Please use the 'new' operator");
    Object.defineProperty(this, '_list', { value: [], writable: true });
    Object.defineProperty(this, '_onChange', { value: typeof onChange === 'function' ? onChange : null });
    if (init === undefined || init === null) return;
    if (typeof init === 'string') this._parse(init);
    else if (init instanceof URLSearchParams) this._list = init._list.map(function (p) { return p.slice(); });
    else if (typeof init === 'object' && typeof init[Symbol.iterator] === 'function') { for (var pair of init) { pair = Array.from(pair); if (pair.length !== 2) throw new TypeError("Failed to construct 'URLSearchParams': Sequence initializer must only contain pair elements"); this._list.push([String(pair[0]), String(pair[1])]); } }
    else if (typeof init === 'object') { var self = this; Object.keys(init).forEach(function (k) { self._list.push([k, String(init[k])]); }); }
  }
  URLSearchParams.prototype = {
    constructor: URLSearchParams,
    _parse: function (s) { s = String(s); if (s[0] === '?') s = s.slice(1); this._list = []; if (!s) return; var self = this; s.split('&').forEach(function (p) { if (!p) return; var i = p.indexOf('='); self._list.push(i >= 0 ? [formDecode(p.slice(0, i)), formDecode(p.slice(i + 1))] : [formDecode(p), '']); }); },
    _update: function () { if (this._onChange) this._onChange(this.toString()); },
    append: function (k, v) { this._list.push([String(k), String(v)]); this._update(); },
    delete: function (k, v) { k = String(k); this._list = this._list.filter(function (p) { return !(p[0] === k && (v === undefined || p[1] === String(v))); }); this._update(); },
    get: function (k) { k = String(k); for (var i = 0; i < this._list.length; i++) if (this._list[i][0] === k) return this._list[i][1]; return null; },
    getAll: function (k) { k = String(k); return this._list.filter(function (p) { return p[0] === k; }).map(function (p) { return p[1]; }); },
    has: function (k, v) { k = String(k); return this._list.some(function (p) { return p[0] === k && (v === undefined || p[1] === String(v)); }); },
    set: function (k, v) { k = String(k); v = String(v); var found = false; this._list = this._list.filter(function (p) { if (p[0] !== k) return true; if (found) return false; found = true; p[1] = v; return true; }); if (!found) this._list.push([k, v]); this._update(); },
    sort: function () { this._list.sort(function (a, b) { return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0; }); this._update(); },
    forEach: function (fn, thisArg) { var self = this; this._list.slice().forEach(function (p) { fn.call(thisArg, p[1], p[0], self); }); },
    keys: function () { return this._list.map(function (p) { return p[0]; }).values(); },
    values: function () { return this._list.map(function (p) { return p[1]; }).values(); },
    entries: function () { return this._list.map(function (p) { return p.slice(); }).values(); },
    get size() { return this._list.length; },
    toString: function () { return this._list.map(function (p) { return formEncode(p[0]) + '=' + formEncode(p[1]); }).join('&'); }
  };
  URLSearchParams.prototype[Symbol.iterator] = URLSearchParams.prototype.entries;
  setTag(URLSearchParams, 'URLSearchParams'); global.URLSearchParams = URLSearchParams;

  // ---- Blob / File / FileReader
  function Blob(parts, opts) {
    if (!(this instanceof Blob)) throw new TypeError("Failed to construct 'Blob': Please use the 'new' operator");
    var chunks = [], total = 0;
    (parts || []).forEach(function (p) { var b = p instanceof Blob ? p._bytes : toBytes(typeof p === 'string' || p instanceof ArrayBuffer || ArrayBuffer.isView(p) ? p : String(p)); chunks.push(b); total += b.length; });
    var bytes = new Uint8Array(total), off = 0;
    chunks.forEach(function (c) { bytes.set(c, off); off += c.length; });
    Object.defineProperty(this, '_bytes', { value: bytes, configurable: true });
    Object.defineProperty(this, 'size', { value: total, enumerable: true, configurable: true });
    Object.defineProperty(this, 'type', { value: String((opts && opts.type) || '').toLowerCase(), enumerable: true });
  }
  Blob.prototype = {
    constructor: Blob,
    text: function () { return Promise.resolve(utf8Decode(this._bytes)); },
    arrayBuffer: function () { return Promise.resolve(this._bytes.slice().buffer); },
    bytes: function () { return Promise.resolve(this._bytes.slice()); },
    slice: function (s, e, type) { var b = new Blob([], { type: type }); Object.defineProperty(b, '_bytes', { value: this._bytes.slice(s, e) }); Object.defineProperty(b, 'size', { value: b._bytes.length, enumerable: true }); return b; },
    stream: function () { return bytesStream(this._bytes); }
  };
  setTag(Blob, 'Blob'); global.Blob = Blob;
  function File(parts, name, opts) { if (!(this instanceof File)) throw new TypeError("Failed to construct 'File': Please use the 'new' operator"); Blob.call(this, parts, opts); Object.defineProperty(this, 'name', { value: String(name), enumerable: true }); Object.defineProperty(this, 'lastModified', { value: opts && opts.lastModified !== undefined ? opts.lastModified : Date.now(), enumerable: true }); this.webkitRelativePath = ''; }
  extend(File, Blob); setTag(File, 'File'); global.File = File;
  function FileReader() { EventTarget.call(this); this.readyState = 0; this.result = null; this.error = null; }
  extend(FileReader, EventTarget);
  function readerRead(r, blob, conv) { r.readyState = 1; setTimeoutImpl(function () { r.result = conv(blob); r.readyState = 2; fire(r, 'loadstart', {}); fire(r, 'progress', {}); fire(r, 'load', {}); fire(r, 'loadend', {}); }, [], 0, false); }
  methods(FileReader.prototype, {
    readAsText: function (b) { readerRead(this, b, function (b) { return utf8Decode(b._bytes); }); },
    readAsArrayBuffer: function (b) { readerRead(this, b, function (b) { return b._bytes.slice().buffer; }); },
    readAsDataURL: function (b) { readerRead(this, b, function (b) { var s = ''; for (var i = 0; i < b._bytes.length; i++) s += String.fromCharCode(b._bytes[i]); return 'data:' + (b.type || 'application/octet-stream') + ';base64,' + global.btoa(s); }); },
    readAsBinaryString: function (b) { readerRead(this, b, function (b) { var s = ''; for (var i = 0; i < b._bytes.length; i++) s += String.fromCharCode(b._bytes[i]); return s; }); },
    abort: function () { this.readyState = 2; fire(this, 'abort', {}); }
  });
  defineHandlerProps(FileReader.prototype, ['load', 'loadstart', 'loadend', 'progress', 'error', 'abort']);
  constant(FileReader, 'EMPTY', 0); constant(FileReader, 'LOADING', 1); constant(FileReader, 'DONE', 2);
  setTag(FileReader, 'FileReader'); global.FileReader = FileReader;
  // Real streams live in platformjs/streams.js and load on first use.
  function bytesStream(bytes) { return new global.ReadableStream({ start: function (c) { if (bytes.length) c.enqueue(bytes); c.close(); } }); }

  // ---- FormData
  // Extension hooks for the platform layer (platformjs/*.js).
  var swHooks = {};
  Object.defineProperty(global, '__swHooks', { value: swHooks });
  function FormData(form) {
    if (!(this instanceof FormData)) throw new TypeError("Failed to construct 'FormData': Please use the 'new' operator");
    Object.defineProperty(this, '_list', { value: [], writable: true });
    if (form && form.nodeType === 1 && form.localName === 'form') {
      var self = this;
      itemsOf(form.elements).forEach(function (c) {
        var name = c.getAttribute('name'); if (!name || c.hasAttribute('disabled')) return;
        var tag = c.localName;
        if (tag === 'input') { var t = c.type; if ((t === 'checkbox' || t === 'radio') && !c.checked) return; if (t === 'submit' || t === 'button' || t === 'reset' || t === 'image') return; if (t === 'file') { (c.files && c.files.length ? Array.from(c.files) : []).forEach(function (f) { self._list.push([name, f]); }); return; } self._list.push([name, c.value]); }
        else if (tag === 'select') { itemsOf(c.options).forEach(function (o) { if (o.selected) self._list.push([name, o.value]); }); }
        else if (tag === 'textarea') self._list.push([name, c.value]);
      });
      if (swHooks.formData) swHooks.formData(form, self._list);
    }
  }
  function fdValue(v, filename) { if (v instanceof Blob) { if (v instanceof File && filename === undefined) return v; return new File([v], filename === undefined ? 'blob' : String(filename), { type: v.type }); } return String(v); }
  FormData.prototype = {
    constructor: FormData,
    append: function (k, v, fn) { this._list.push([String(k), fdValue(v, fn)]); },
    set: function (k, v, fn) { k = String(k); v = fdValue(v, fn); var found = false; this._list = this._list.filter(function (p) { if (p[0] !== k) return true; if (found) return false; found = true; p[1] = v; return true; }); if (!found) this._list.push([k, v]); },
    get: URLSearchParams.prototype.get, getAll: URLSearchParams.prototype.getAll, has: function (k) { return URLSearchParams.prototype.has.call(this, k); },
    delete: function (k) { k = String(k); this._list = this._list.filter(function (p) { return p[0] !== k; }); },
    forEach: URLSearchParams.prototype.forEach, keys: URLSearchParams.prototype.keys, values: URLSearchParams.prototype.values, entries: URLSearchParams.prototype.entries
  };
  FormData.prototype[Symbol.iterator] = FormData.prototype.entries;
  setTag(FormData, 'FormData'); global.FormData = FormData;

  // ---- Headers
  function Headers(init) {
    if (!(this instanceof Headers)) throw new TypeError("Failed to construct 'Headers': Please use the 'new' operator");
    Object.defineProperty(this, '_map', { value: new Map() });
    if (!init) return;
    var self = this;
    if (init instanceof Headers) init.forEach(function (v, k) { self.append(k, v); });
    else if (Array.isArray(init) || (typeof init === 'object' && typeof init[Symbol.iterator] === 'function')) { for (var pair of init) { pair = Array.from(pair); if (pair.length !== 2) throw new TypeError('Invalid header pair'); self.append(pair[0], pair[1]); } }
    else if (typeof init === 'object') Object.keys(init).forEach(function (k) { self.append(k, init[k]); });
  }
  function hname(n) { n = String(n).toLowerCase(); if (!/^[!#$%&'*+\-.^_`|~0-9a-z]+$/.test(n)) throw new TypeError('Invalid name: ' + n); return n; }
  function hvalue(v) { return String(v).replace(/^[\t ]+|[\t ]+$/g, ''); }
  Headers.prototype = {
    constructor: Headers,
    append: function (k, v) { k = hname(k); v = hvalue(v); var cur = this._map.get(k); this._map.set(k, cur === undefined ? v : (k === 'set-cookie' ? cur + '\n' + v : cur + ', ' + v)); },
    set: function (k, v) { this._map.set(hname(k), hvalue(v)); },
    get: function (k) { var v = this._map.get(hname(k)); return v === undefined ? null : v.replace(/\n/g, ', '); },
    has: function (k) { return this._map.has(hname(k)); },
    delete: function (k) { this._map.delete(hname(k)); },
    getSetCookie: function () { var v = this._map.get('set-cookie'); return v ? v.split('\n') : []; },
    forEach: function (fn, thisArg) { var self = this; Array.from(this._map.keys()).sort().forEach(function (k) { fn.call(thisArg, self.get(k), k, self); }); },
    keys: function () { return Array.from(this._map.keys()).sort().values(); },
    values: function () { var self = this; return Array.from(this._map.keys()).sort().map(function (k) { return self.get(k); }).values(); },
    entries: function () { var self = this; return Array.from(this._map.keys()).sort().map(function (k) { return [k, self.get(k)]; }).values(); }
  };
  Headers.prototype[Symbol.iterator] = Headers.prototype.entries;
  setTag(Headers, 'Headers'); global.Headers = Headers;

  // ---- AbortController / AbortSignal
  function AbortSignal() { EventTarget.call(this); this.aborted = false; this.reason = undefined; }
  extend(AbortSignal, EventTarget);
  methods(AbortSignal.prototype, { throwIfAborted: function () { if (this.aborted) throw this.reason; } });
  defineHandlerProps(AbortSignal.prototype, ['abort']);
  function abortSignal(sig, reason) { if (sig.aborted) return; sig.aborted = true; sig.reason = reason === undefined ? DOMException('signal is aborted without reason', 'AbortError') : reason; fire(sig, 'abort', { bubbles: false, cancelable: false }); }
  AbortSignal.abort = function (reason) { var s = new AbortSignal(); s.aborted = true; s.reason = reason === undefined ? DOMException('signal is aborted without reason', 'AbortError') : reason; return s; };
  AbortSignal.timeout = function (ms) { var s = new AbortSignal(); setTimeoutImpl(function () { abortSignal(s, DOMException('signal timed out', 'TimeoutError')); }, [], ms, false); return s; };
  AbortSignal.any = function (signals) { var s = new AbortSignal(); Array.from(signals).forEach(function (x) { if (x.aborted) { if (!s.aborted) abortSignal(s, x.reason); } else x.addEventListener('abort', function () { abortSignal(s, x.reason); }); }); return s; };
  setTag(AbortSignal, 'AbortSignal'); global.AbortSignal = AbortSignal;
  function AbortController() { if (!(this instanceof AbortController)) throw new TypeError("Failed to construct 'AbortController': Please use the 'new' operator"); Object.defineProperty(this, 'signal', { value: new AbortSignal(), enumerable: true }); }
  methods(AbortController.prototype, { abort: function (reason) { abortSignal(this.signal, reason); } });
  setTag(AbortController, 'AbortController'); global.AbortController = AbortController;

  // ---- Body mixin shared by Request/Response
  function bodyToString(body) {
    if (body === null || body === undefined) return '';
    if (typeof body === 'string') return body;
    if (body instanceof URLSearchParams) return body.toString();
    if (body instanceof Blob) return utf8Decode(body._bytes);
    if (body instanceof ArrayBuffer || ArrayBuffer.isView(body)) return utf8Decode(toBytes(body));
    if (body instanceof FormData) { var b = '----SweefFormBoundary' + Math.random().toString(16).slice(2); var s = ''; body._list.forEach(function (p) { s += '--' + b + '\r\nContent-Disposition: form-data; name="' + p[0] + '"'; if (p[1] instanceof File) { s += '; filename="' + p[1].name + '"\r\nContent-Type: ' + (p[1].type || 'application/octet-stream') + '\r\n\r\n' + utf8Decode(p[1]._bytes) + '\r\n'; } else s += '\r\n\r\n' + p[1] + '\r\n'; }); return s + '--' + b + '--\r\n'; }
    if (body && typeof body.getReader === 'function') return '';
    return String(body);
  }
  function bodyContentType(body) {
    if (typeof body === 'string') return 'text/plain;charset=UTF-8';
    if (body instanceof URLSearchParams) return 'application/x-www-form-urlencoded;charset=UTF-8';
    if (body instanceof Blob) return body.type || null;
    if (body instanceof FormData) return 'multipart/form-data; boundary=' + (/boundary=(\S+)/.exec(bodyToString(body)) || [])[1];
    return null;
  }
  function defineBody(proto) {
    getters(proto, { bodyUsed: { get: function () { return !!this._used; } }, body: { get: function () { if (this._body === null || this._body === undefined) return null; if (typeof this._body.getReader === 'function') return this._body; return bytesStream(this._rawBytes ? this._rawBytes.slice() : utf8Encode(this._text())); } } });
    methods(proto, {
      _text: function () { return bodyToString(this._body); },
      _consume: function () {
        if (this._used) return Promise.reject(new TypeError('Failed to execute \'text\' on \'' + this.constructor.name + '\': body stream already read'));
        this._used = true;
        var self = this, b = this._body;
        if (b && typeof b.getReader === 'function') {
          var rd = b.getReader(), parts = [], total = 0;
          var pump = function () { return rd.read().then(function (r) {
            if (r.done) { var all = new Uint8Array(total), o = 0; parts.forEach(function (p) { all.set(p, o); o += p.length; }); self._rawBytes = all; return utf8Decode(all); }
            var v = r.value, u = typeof v === 'string' ? utf8Encode(v) : v instanceof ArrayBuffer ? new Uint8Array(v) : ArrayBuffer.isView(v) ? new Uint8Array(v.buffer, v.byteOffset, v.byteLength) : null;
            if (!u) throw new TypeError('Received non-Uint8Array chunk');
            parts.push(u); total += u.length; return pump();
          }); };
          return pump();
        }
        return Promise.resolve(this._text());
      },
      text: function () { return this._consume(); },
      json: function () { return this._consume().then(function (t) { return JSON.parse(t); }); },
      arrayBuffer: function () { var self = this; return this._consume().then(function (t) { var raw = self._rawBytes; return raw ? raw.slice().buffer : utf8Encode(t).buffer; }); },
      bytes: function () { var self = this; return this._consume().then(function (t) { var raw = self._rawBytes; return raw ? raw.slice() : utf8Encode(t); }); },
      blob: function () { var type = this.headers.get('content-type') || '', self = this; return this._consume().then(function (t) { var raw = self._rawBytes; return new Blob([raw || t], { type: type }); }); },
      formData: function () { var type = this.headers.get('content-type') || ''; return this._consume().then(function (t) { var fd = new FormData(); if (/urlencoded/.test(type)) new URLSearchParams(t).forEach(function (v, k) { fd.append(k, v); }); return fd; }); }
    });
  }
  function Request(input, init) {
    if (!(this instanceof Request)) throw new TypeError("Failed to construct 'Request': Please use the 'new' operator");
    init = init || {};
    var url, method = 'GET', headers = null, body = null, extra = {};
    if (input instanceof Request) { url = input.url; method = input.method; headers = new Headers(input.headers); body = input._body; extra = { mode: input.mode, credentials: input.credentials, cache: input.cache, redirect: input.redirect, referrer: input.referrer, integrity: input.integrity, signal: input.signal, destination: input.destination }; }
    else url = new URL(String(input), currentURL()).href;
    if (init.method !== undefined) method = String(init.method).toUpperCase() === String(init.method).toUpperCase() && /^(get|post|put|delete|head|options|patch)$/i.test(init.method) ? String(init.method).toUpperCase() : String(init.method);
    if (init.headers !== undefined) headers = new Headers(init.headers);
    if (init.body !== undefined && init.body !== null) { if (method === 'GET' || method === 'HEAD') throw new TypeError("Failed to construct 'Request': Request with GET/HEAD method cannot have body."); body = init.body; }
    headers = headers || new Headers();
    if (body !== null && !headers.has('content-type')) { var ct = bodyContentType(body); if (ct) headers.set('content-type', ct); }
    Object.defineProperty(this, 'url', { value: url, enumerable: true });
    Object.defineProperty(this, 'method', { value: method, enumerable: true });
    Object.defineProperty(this, 'headers', { value: headers, enumerable: true });
    Object.defineProperty(this, '_body', { value: body, writable: true });
    Object.defineProperty(this, '_used', { value: false, writable: true });
    var self = this;
    ['mode', 'credentials', 'cache', 'redirect', 'referrer', 'integrity', 'keepalive', 'priority'].forEach(function (k) { var dflt = { mode: 'cors', credentials: 'same-origin', cache: 'default', redirect: 'follow', referrer: 'about:client', integrity: '', keepalive: false, priority: 'auto' }[k]; Object.defineProperty(self, k, { value: init[k] !== undefined ? init[k] : extra[k] !== undefined ? extra[k] : dflt, enumerable: true }); });
    Object.defineProperty(this, 'signal', { value: init.signal || extra.signal || new AbortSignal(), enumerable: true });
    Object.defineProperty(this, 'destination', { value: init.__destination !== undefined ? String(init.__destination) : extra.destination || '', enumerable: true }); // __destination: set by the service worker host for the requests it hands to fetch events
    Object.defineProperty(this, 'referrerPolicy', { value: init.referrerPolicy || '', enumerable: true });
  }
  defineBody(Request.prototype);
  methods(Request.prototype, { clone: function () { return new Request(this); } });
  setTag(Request, 'Request'); global.Request = Request;
  function Response(body, init) {
    if (!(this instanceof Response)) throw new TypeError("Failed to construct 'Response': Please use the 'new' operator");
    init = init || {};
    var status = init.status === undefined ? 200 : init.status | 0;
    if (status < 200 || status > 599) throw new RangeError("Failed to construct 'Response': The status provided (" + status + ") is outside the range [200, 599].");
    var headers = new Headers(init.headers);
    if (body !== undefined && body !== null && !headers.has('content-type')) { var ct = bodyContentType(body); if (ct) headers.set('content-type', ct); }
    Object.defineProperty(this, 'status', { value: status, enumerable: true, configurable: true });
    Object.defineProperty(this, 'statusText', { value: init.statusText === undefined ? '' : String(init.statusText), enumerable: true });
    Object.defineProperty(this, 'headers', { value: headers, enumerable: true });
    Object.defineProperty(this, 'url', { value: init.url || '', enumerable: true, writable: true });
    Object.defineProperty(this, 'type', { value: init.type || 'default', enumerable: true, writable: true });
    Object.defineProperty(this, 'redirected', { value: !!init.redirected, enumerable: true, writable: true });
    Object.defineProperty(this, '_body', { value: body === undefined ? null : body, writable: true });
    Object.defineProperty(this, '_used', { value: false, writable: true });
  }
  defineBody(Response.prototype);
  getters(Response.prototype, { ok: { get: function () { return this.status >= 200 && this.status < 300; } } });
  methods(Response.prototype, { clone: function () { if (this._used) throw new TypeError("Failed to execute 'clone' on 'Response': Response body is already used"); var r = new Response(this._body, { status: this.status, statusText: this.statusText, headers: this.headers, url: this.url, type: this.type, redirected: this.redirected }); if (this._rawBytes) Object.defineProperty(r, '_rawBytes', { value: this._rawBytes, writable: true, configurable: true }); return r; } });
  Response.error = function () { var r = new Response(null, { status: 200, type: 'error' }); Object.defineProperty(r, 'status', { value: 0 }); return r; };
  Response.redirect = function (url, status) { return new Response(null, { status: status || 302, headers: { location: new URL(url, currentURL()).href } }); };
  Response.json = function (data, init) { init = Object.assign({}, init || {}); var h = new Headers(init.headers); if (!h.has('content-type')) h.set('content-type', 'application/json'); init.headers = h; return new Response(JSON.stringify(data), init); };
  setTag(Response, 'Response'); global.Response = Response;

  // ---- fetch: an asynchronous request behind a Promise, started on a microtask
  // The request body as the host takes it: text as a string, bytes (ArrayBuffer, typed array, Blob) as
  // {bin: Uint8Array} so that they are not mangled by a trip through UTF-8.
  function sendBody(body) {
    if (body === null || body === undefined) return null;
    if (body instanceof ArrayBuffer) return { bin: new Uint8Array(body) };
    if (ArrayBuffer.isView(body)) return { bin: new Uint8Array(body.buffer, body.byteOffset, body.byteLength) };
    if (body instanceof Blob) return { bin: body._bytes };
    return bodyToString(body);
  }
  function bodyWire(body) {
    if (body && typeof body === 'object' && body.bin) {
      var u = body.bin, s = '';
      for (var i = 0; i < u.length; i += 0x6000) s += String.fromCharCode.apply(null, u.subarray(i, i + 0x6000));
      return [global.btoa(s), true];
    }
    return [body, false];
  }
  function doHttp(method, url, headers, body) {
    var hobj = {};
    if (headers) headers.forEach(function (v, k) { hobj[k] = v; });
    var w = bodyWire(body);
    var r = sw.http(method, url, hobj, w[0], w[1]);
    if (!r || typeof r !== 'object') throw new TypeError('Failed to fetch');
    return r;
  }
  // Asynchronous requests: Go makes them on a goroutine and the event-loop pump calls __swHttpEvent when one is
  // answered. cb(result) or cb(null, error). A request made while a module is evaluated completes in place.
  var httpCbs = new Map();
  function doHttpAsync(method, url, headers, body, cb) {
    var hobj = {};
    if (headers) headers.forEach(function (v, k) { hobj[k] = v; });
    var t;
    var w = bodyWire(body);
    try { t = sw.httpAsync(method, url, hobj, w[0], false, w[1]); } catch (e) { cb(null, e); return; }
    if (t && typeof t === 'object') { cb(t); return; }
    httpCbs.set(t, cb);
  }
  Object.defineProperty(global, '__swHttpEvent', { configurable: true, writable: true, enumerable: false, value: function (token) {
    var cb = httpCbs.get(token); httpCbs.delete(token);
    var out = sw.httpTake(token);
    if (!cb) return false;
    try { if (out && out.error) cb(null, new Error(out.error)); else cb(out); } catch (e) { reportError(e); }
    return true;
  } });
  function responseFrom(r, url) {
    var status = r.status | 0;
    var resp = new Response(r.body === undefined || r.body === null ? '' : r.body, { status: status >= 200 && status <= 599 ? status : 200, statusText: r.statusText || '', headers: r.headers || {} });
    if (status < 200 || status > 599) Object.defineProperty(resp, 'status', { value: status });
    if (r.b64) Object.defineProperty(resp, '_rawBytes', { value: b64ToBytes(String(r.b64)), writable: true, configurable: true });
    resp.url = r.url || url;
    resp.redirected = !!r.url && r.url !== url;
    resp.type = 'basic';
    return resp;
  }
  global.fetch = function fetch(input, init) {
    var req;
    try { req = new Request(input, init); } catch (e) { return Promise.reject(e); }
    if (req.signal && req.signal.aborted) return Promise.reject(req.signal.reason);
    return new Promise(function (resolve, reject) {
      var aborted = false;
      if (req.signal) req.signal.addEventListener('abort', function () { aborted = true; reject(req.signal.reason); });
      queueMicrotaskImpl(function () {
        if (aborted) return;
        doHttpAsync(req.method, req.url, req.headers, sendBody(req._body), function (r, e) {
          if (aborted) return;
          if (e) { reject(e instanceof Error ? new TypeError('Failed to fetch: ' + (e.message || e)) : new TypeError('Failed to fetch')); return; }
          if (!r || (!r.status && !r.body)) { reject(new TypeError('Failed to fetch')); return; }
          resolve(responseFrom(r, req.url));
        });
      });
    });
  };

  // ---- XMLHttpRequest (sync and async via timers)
  function XMLHttpRequestEventTarget() { EventTarget.call(this); }
  extend(XMLHttpRequestEventTarget, EventTarget);
  defineHandlerProps(XMLHttpRequestEventTarget.prototype, ['loadstart', 'progress', 'abort', 'error', 'load', 'timeout', 'loadend']);
  global.XMLHttpRequestEventTarget = XMLHttpRequestEventTarget;
  function XMLHttpRequestUpload() { XMLHttpRequestEventTarget.call(this); }
  extend(XMLHttpRequestUpload, XMLHttpRequestEventTarget); global.XMLHttpRequestUpload = XMLHttpRequestUpload;
  function XMLHttpRequest() {
    if (!(this instanceof XMLHttpRequest)) throw new TypeError("Failed to construct 'XMLHttpRequest': Please use the 'new' operator");
    XMLHttpRequestEventTarget.call(this);
    this.readyState = 0; this.status = 0; this.statusText = ''; this.responseURL = ''; this.responseType = ''; this.timeout = 0; this.withCredentials = false;
    this.upload = new XMLHttpRequestUpload();
    this._reqHeaders = new Headers(); this._resHeaders = new Headers(); this._responseText = ''; this._method = 'GET'; this._url = ''; this._async = true; this._sent = false; this._aborted = false; this._timer = 0;
  }
  extend(XMLHttpRequest, XMLHttpRequestEventTarget);
  defineHandlerProps(XMLHttpRequest.prototype, ['readystatechange']);
  function xhrState(x, s) { x.readyState = s; fire(x, 'readystatechange', { bubbles: false, cancelable: false }); }
  function xhrFinish(x, r, url) {
    if (x._aborted) return;
    x.status = r.status | 0; x.statusText = r.statusText || ''; x.responseURL = r.url || url;
    x._resHeaders = new Headers(r.headers || {});
    x._responseText = r.body === undefined || r.body === null ? '' : String(r.body);
    x._responseBytes = r.b64 ? b64ToBytes(String(r.b64)) : null;
    xhrState(x, 2); xhrState(x, 3); xhrState(x, 4);
    var len = x._responseText.length;
    fire(x, 'progress', { lengthComputable: true, loaded: len, total: len });
    fire(x, 'load', { lengthComputable: true, loaded: len, total: len });
    fire(x, 'loadend', { lengthComputable: true, loaded: len, total: len });
  }
  function xhrFail(x, type) {
    if (x._aborted && type !== 'abort') return;
    x.status = 0; x.statusText = ''; x._responseText = '';
    xhrState(x, 4);
    fire(x, type, {}); fire(x, 'loadend', {});
  }
  getters(XMLHttpRequest.prototype, {
    responseText: { get: function () { if (this.responseType && this.responseType !== 'text') throw DOMException("Failed to read the 'responseText' property from 'XMLHttpRequest': The value is only accessible if the object's 'responseType' is '' or 'text' (was '" + this.responseType + "').", 'InvalidStateError'); return this.readyState >= 3 ? this._responseText : ''; } },
    response: {
      get: function () {
        if (this.readyState !== 4) return this.responseType === '' || this.responseType === 'text' ? '' : null;
        switch (this.responseType) {
          case '': case 'text': return this._responseText;
          case 'json': try { return JSON.parse(this._responseText); } catch (e) { return null; }
          case 'arraybuffer': return this._responseBytes ? this._responseBytes.slice().buffer : utf8Encode(this._responseText).buffer;
          case 'blob': return new Blob([this._responseBytes || this._responseText], { type: this._resHeaders.get('content-type') || '' });
          case 'document': return this.responseXML;
          default: return this._responseText;
        }
      }
    },
    responseXML: { get: function () { if (this.readyState !== 4 || !/html|xml/.test(this._resHeaders.get('content-type') || '') && this.responseType !== 'document') return null; try { var ct = this._resHeaders.get('content-type') || ''; return new DOMParser().parseFromString(this._responseText, /xml/.test(ct) && !/html/.test(ct) ? 'text/xml' : 'text/html'); } catch (e) { return null; } } }
  });
  methods(XMLHttpRequest.prototype, {
    open: function (method, url, async, user, password) {
      this._method = String(method).toUpperCase(); this._url = new URL(String(url), currentURL()).href; this._async = async === undefined ? true : !!async;
      if (user) { this._reqHeaders.set('authorization', 'Basic ' + global.btoa(user + ':' + (password || ''))); }
      this._reqHeaders = new Headers(); this._sent = false; this._aborted = false; this._responseText = '';
      xhrState(this, 1);
    },
    setRequestHeader: function (k, v) { if (this.readyState !== 1 || this._sent) throw DOMException("Failed to execute 'setRequestHeader' on 'XMLHttpRequest': The object's state must be OPENED.", 'InvalidStateError'); this._reqHeaders.append(k, v); },
    getResponseHeader: function (k) { return this.readyState < 2 ? null : this._resHeaders.get(k); },
    getAllResponseHeaders: function () { if (this.readyState < 2) return ''; var out = ''; this._resHeaders.forEach(function (v, k) { out += k + ': ' + v + '\r\n'; }); return out; },
    overrideMimeType: function () { },
    send: function (body) {
      if (this.readyState !== 1 || this._sent) throw DOMException("Failed to execute 'send' on 'XMLHttpRequest': The object's state must be OPENED.", 'InvalidStateError');
      this._sent = true;
      if (this._method === 'GET' || this._method === 'HEAD') body = null;
      var bodyStr = body === undefined || body === null ? null : sendBody(body);
      var bodyLen = bodyStr === null ? 0 : typeof bodyStr === 'string' ? bodyStr.length : bodyStr.bin.length;
      if (bodyStr !== null && !this._reqHeaders.has('content-type')) { var ct = bodyContentType(body); if (ct) this._reqHeaders.set('content-type', ct); }
      var self = this, url = this._url, method = this._method, headers = this._reqHeaders;
      fire(this, 'loadstart', {});
      if (bodyStr !== null) fire(this.upload, 'loadstart', {});
      var done = function (r, e) {
        if (self._aborted || self.readyState === 4) return;
        if (e) { xhrFail(self, 'error'); return; }
        if (bodyStr !== null) { fire(self.upload, 'progress', { lengthComputable: true, loaded: bodyLen, total: bodyLen }); fire(self.upload, 'load', {}); fire(self.upload, 'loadend', {}); }
        if (!r || (!r.status && !r.body)) { xhrFail(self, 'error'); return; }
        xhrFinish(self, r, url);
      };
      if (this._async) {
        this._timer = setTimeoutImpl(function () { if (!self._aborted) doHttpAsync(method, url, headers, bodyStr, done); }, [], 0, false);
        if (this.timeout > 0) { var t = this.timeout; setTimeoutImpl(function () { if (self.readyState !== 4 && !self._aborted) { clearTimer(self._timer); self._aborted = true; xhrFail(self, 'timeout'); } }, [], t, false); }
      } else {
        var r, err = null;
        try { r = doHttp(method, url, headers, bodyStr); } catch (e) { err = e; }
        done(r, err);
      }
    },
    abort: function () { if (this.readyState === 0 || this.readyState === 4 && !this._sent) { this.readyState = 0; return; } this._aborted = true; clearTimer(this._timer); if (this._sent) xhrFail(this, 'abort'); this.readyState = 0; }
  });
  [['UNSENT', 0], ['OPENED', 1], ['HEADERS_RECEIVED', 2], ['LOADING', 3], ['DONE', 4]].forEach(function (p) { constant(XMLHttpRequest, p[0], p[1]); constant(XMLHttpRequest.prototype, p[0], p[1]); });
  setTag(XMLHttpRequest, 'XMLHttpRequest'); global.XMLHttpRequest = XMLHttpRequest;
  global.ProgressEvent = global.ProgressEvent;

  // ==== 10. Lifecycle & exports for Go =====================================
  // FileList: an immutable list of File objects (input.files, DataTransfer.files)
  function FileList() { illegal(); }
  FileList.prototype.item = function item(i) { i = i >>> 0; return this[i] || null; };
  Object.defineProperty(FileList.prototype, 'length', { get: function () { return this._n; }, enumerable: true, configurable: true });
  FileList.prototype[Symbol.iterator] = function () { var a = []; for (var k = 0; k < this._n; k++) a.push(this[k]); return a[Symbol.iterator](); };
  setTag(FileList, 'FileList'); global.FileList = FileList;
  function makeFileList(files) {
    var l = Object.create(FileList.prototype);
    def(l, '_n', { value: files.length, enumerable: false });
    for (var k = 0; k < files.length; k++) def(l, k, { value: files[k], enumerable: true });
    return l;
  }
  // DataTransfer (drag and drop, clipboard events): string items by type, file items, in insertion order
  var DT_STATE = new WeakMap();
  function dtNorm(fmt) { fmt = String(fmt).toLowerCase(); return fmt === 'text' ? 'text/plain' : fmt === 'url' ? 'text/uri-list' : fmt; }
  function dtList(dt) { var s = DT_STATE.get(dt); if (!s) throw new TypeError('Illegal invocation'); return s; }
  function DataTransfer() {
    if (!new.target) throw new TypeError("Failed to construct 'DataTransfer': Please use the 'new' operator, this DOM object constructor cannot be called as a function.");
    DT_STATE.set(this, { list: [], readOnly: false, itemList: null });
    this.dropEffect = 'none'; this.effectAllowed = 'all';
  }
  function DataTransferItem() { illegal(); }
  function DataTransferItemList() { illegal(); }
  function makeItem(dt, rec) {
    var it = Object.create(DataTransferItem.prototype);
    def(it, '_', { value: { dt: dt, rec: rec } });
    return it;
  }
  function liveItems(dt) { return dtList(dt).list; }
  Object.defineProperties(DataTransferItem.prototype, {
    kind: { get: function () { return this._.rec.kind; }, enumerable: true, configurable: true },
    type: { get: function () { return this._.rec.type; }, enumerable: true, configurable: true }
  });
  DataTransferItem.prototype.getAsString = function getAsString(cb) {
    if (cb === null || cb === undefined) return;
    if (typeof cb !== 'function') throw new TypeError("Failed to execute 'getAsString' on 'DataTransferItem': The callback provided as parameter 1 is not a function.");
    var r = this._.rec; if (r.kind !== 'string') return;
    var v = r.data; setTimeoutImpl(function () { cb(v); }, [], 0, false);
  };
  DataTransferItem.prototype.getAsFile = function getAsFile() { return this._.rec.kind === 'file' ? this._.rec.file : null; };
  DataTransferItem.prototype.webkitGetAsEntry = function webkitGetAsEntry() { return null; };
  setTag(DataTransferItem, 'DataTransferItem'); global.DataTransferItem = DataTransferItem;
  Object.defineProperty(DataTransferItemList.prototype, 'length', { get: function () { return liveItems(this._dt).length; }, enumerable: true, configurable: true });
  DataTransferItemList.prototype.add = function add(data, type) {
    var dt = this._dt, st = dtList(dt);
    if (st.readOnly) return null;
    if (typeof File === 'function' && data instanceof File) {
      var rec = { kind: 'file', type: String(data.type || '').toLowerCase(), file: data };
      st.list.push(rec); return makeItem(dt, rec);
    }
    if (arguments.length < 2) throw new TypeError("Failed to execute 'add' on 'DataTransferItemList': parameter 1 is not of type 'File'.");
    type = dtNorm(type);
    for (var i = 0; i < st.list.length; i++) if (st.list[i].kind === 'string' && st.list[i].type === type) throw new global.DOMException("An item already exists for type '" + type + "'.", 'NotSupportedError');
    var r = { kind: 'string', type: type, data: String(data) };
    st.list.push(r); return makeItem(dt, r);
  };
  DataTransferItemList.prototype.remove = function remove(i) { var st = dtList(this._dt); if (st.readOnly) throw new global.DOMException('The list is not writable.', 'InvalidStateError'); i = i >>> 0; if (i < st.list.length) st.list.splice(i, 1); };
  DataTransferItemList.prototype.clear = function clear() { var st = dtList(this._dt); if (!st.readOnly) st.list.length = 0; };
  DataTransferItemList.prototype[Symbol.iterator] = function () { var dt = this._dt, a = liveItems(dt).map(function (r) { return makeItem(dt, r); }); return a[Symbol.iterator](); };
  setTag(DataTransferItemList, 'DataTransferItemList'); global.DataTransferItemList = DataTransferItemList;
  function itemListOf(dt) {
    var st = dtList(dt);
    if (st.itemList) return st.itemList;
    var l = Object.create(DataTransferItemList.prototype);
    def(l, '_dt', { value: dt });
    // index access: a Proxy keeps l[0], l[1] live
    st.itemList = typeof Proxy === 'function' ? new Proxy(l, { get: function (t, k, r) { if (typeof k === 'string' && /^\d+$/.test(k)) { var rec = liveItems(dt)[+k]; return rec ? makeItem(dt, rec) : undefined; } return Reflect.get(t, k, r); }, has: function (t, k) { return typeof k === 'string' && /^\d+$/.test(k) ? +k < liveItems(dt).length : k in t; } }) : l;
    return st.itemList;
  }
  DataTransfer.prototype.setData = function setData(fmt, data) {
    var st = dtList(this); if (st.readOnly) return;
    fmt = dtNorm(fmt); data = String(data);
    for (var i = 0; i < st.list.length; i++) if (st.list[i].kind === 'string' && st.list[i].type === fmt) { st.list[i].data = data; return; }
    st.list.push({ kind: 'string', type: fmt, data: data });
  };
  DataTransfer.prototype.getData = function getData(fmt) {
    var st = dtList(this); fmt = dtNorm(fmt);
    for (var i = 0; i < st.list.length; i++) if (st.list[i].kind === 'string' && st.list[i].type === fmt) return st.list[i].data;
    return '';
  };
  DataTransfer.prototype.clearData = function clearData(fmt) {
    var st = dtList(this); if (st.readOnly) return;
    if (fmt === undefined) st.list = st.list.filter(function (r) { return r.kind !== 'string'; });
    else { var f = dtNorm(fmt); st.list = st.list.filter(function (r) { return !(r.kind === 'string' && r.type === f); }); }
    if (st.itemList) { /* the live view reads st.list */ }
  };
  DataTransfer.prototype.setDragImage = function setDragImage() { };
  Object.defineProperties(DataTransfer.prototype, {
    types: { get: function () { var st = dtList(this), out = []; st.list.forEach(function (r) { if (r.kind === 'string') out.push(r.type); }); if (st.list.some(function (r) { return r.kind === 'file'; })) out.push('Files'); return Object.freeze(out); }, enumerable: true, configurable: true },
    items: { get: function () { return itemListOf(this); }, enumerable: true, configurable: true },
    files: { get: function () { return makeFileList(dtList(this).list.filter(function (r) { return r.kind === 'file'; }).map(function (r) { return r.file; })); }, enumerable: true, configurable: true }
  });
  setTag(DataTransfer, 'DataTransfer'); global.DataTransfer = DataTransfer;
  // the host builds a DataTransfer for clipboard events: entries are {type, text} or {type, file}; readOnly while a paste handler reads it
  exportFn('__swMakeDataTransfer', function (entries, readOnly) {
    var dt = new DataTransfer(), st = dtList(dt);
    (entries || []).forEach(function (e) {
      if (e.file) st.list.push({ kind: 'file', type: String(e.type).toLowerCase(), file: e.file });
      else st.list.push({ kind: 'string', type: dtNorm(e.type), data: String(e.text) });
    });
    st.readOnly = !!readOnly;
    return dt;
  });
  exportFn('__swDataTransferEntries', function (dt) {
    var out = [];
    dtList(dt).list.forEach(function (r) { if (r.kind === 'string') out.push({ type: r.type, text: r.data }); else out.push({ type: r.type, file: r.file }); });
    return out;
  });
  global.ClipboardEvent.prototype.clipboardData = null;

  function targetOf(id) { if (id === 0 || id === null || id === undefined) return documentWrapper; if (id === -1) return global; return wrap(+id) || documentWrapper; }

  function dispatchFromGo(id, type, init) {
    var target = targetOf(id);
    if (!target) return true;
    type = String(type);
    init = init && typeof init === 'object' ? init : {};
    if (type === 'click' && target.nodeType === 1) {
      var revert = preClick(target);
      var ok = fire(target, type, Object.assign({ button: 0, buttons: 0, detail: 1 }, init));
      if (!ok && revert) revert();
      return ok;
    }
    if (type === 'focus') { focusElement(target); return true; }
    if (type === 'blur') { if (activeElement === target) blurActive(null); return true; }
    if (type === 'submit' && target.localName === 'form') { return fire(target, 'submit', Object.assign({ bubbles: true, cancelable: true }, init)); }
    if (type === 'change' && target.localName === 'select' && init.value !== undefined) { sw.setValue(target[ID], String(init.value)); }
    return fire(target, type, init);
  }

  // keyboard: keydown -> (keypress + beforeinput + value update + input for printable) -> keyup
  var PRINTABLE_INPUT = /^(text|password|email|number|tel|url|search|date|time|datetime-local|month|week|color|hidden)$/;
  function dispatchKey(id, key, mods) {
    // no target: the focused element, as for a real key press (the page body when nothing has focus)
    var el = (id === 0 || id === null || id === undefined) && activeElement && activeElement.isConnected ? activeElement : targetOf(id);
    if (!el) return true;
    key = String(key); mods = mods || {};
    var init = Object.assign({ key: key, code: codeFor(key), bubbles: true, cancelable: true }, mods);
    if (!el.isConnected && el !== documentWrapper) return true;
    if (el.nodeType === 1 && el !== activeElement) focusElement(el);
    var ok = fire(el, 'keydown', init);
    if (!ok) { fire(el, 'keyup', Object.assign({}, init, { cancelable: false })); return false; }
    var editable = el.nodeType === 1 && ((el.localName === 'input' && PRINTABLE_INPUT.test(el.type)) || el.localName === 'textarea' || el.isContentEditable);
    // a character with Control or Meta held is a shortcut (Control+a), not text
    var printable = !(mods.ctrlKey || mods.metaKey) && (key.length === 1 || key === 'Enter' && (el.localName === 'textarea' || el.isContentEditable));
    var result = true, used = false, custom = typeof global.__swKeyDefault === 'function';
    if (printable) {
      ok = fire(el, 'keypress', Object.assign({}, init, { charCode: key.length === 1 ? key.charCodeAt(0) : 13 }));
      if (ok && custom) { used = true; result = global.__swKeyDefault(el, key, mods); }
      else if (ok && editable) { used = true; result = editInput(el, 'insertText', key === 'Enter' ? '\n' : key); }
    } else if (custom) {
      used = !!global.__swKeyDefault(el, key, mods);
    } else if (key === 'Backspace' && editable) { used = true; result = editInput(el, 'deleteContentBackward', null); }
    else if (key === 'Delete' && editable) { used = true; result = editInput(el, 'deleteContentForward', null); }
    if (!used && !printable) {
      if (key === 'Tab' && !mods.ctrlKey && !mods.metaKey && !mods.altKey && host('nextTabStop')) {
        // sequential focus navigation: the keyup goes to the newly focused element, as in a browser
        var nid = sw.nextTabStop(el.nodeType === 1 && el === activeElement ? el[ID] : 0, !!mods.shiftKey), next = nid ? wrap(+nid) : null;
        if (next && next !== el) {
          focusElement(next);
          if ((next.localName === 'input' && PRINTABLE_INPUT.test(next.type)) || next.localName === 'textarea') { var nl = next.value.length; slots(next).selStart = 0; slots(next).selEnd = nl; }
          el = next;
        }
      } else if (key === 'Enter' && el.nodeType === 1) {
        if (el.localName === 'input') { var f = el.form; if (f) submitForm(f, queryOne(f, 'button[type=submit],input[type=submit],button:not([type])') || null); }
        else if (el.localName === 'button' || el.localName === 'a' || el.getAttribute('role') === 'button') clickElement(el);
      } else if (key === ' ' && el.nodeType === 1 && (el.localName === 'button' || (el.localName === 'input' && (el.type === 'checkbox' || el.type === 'radio' || el.type === 'button' || el.type === 'submit')))) clickElement(el);
    }
    fire(el, 'keyup', Object.assign({}, init, { cancelable: false }));
    return result;
  }
  function editInput(el, inputType, data) {
    if (!fire(el, 'beforeinput', { bubbles: true, cancelable: true, inputType: inputType, data: data })) return false;
    if (el.isContentEditable && el.localName !== 'input' && el.localName !== 'textarea') {
      var t = el.textContent;
      if (inputType === 'insertText') t += data; else if (inputType === 'deleteContentBackward') t = t.slice(0, -1);
      el.textContent = t;
    } else {
      var v = el.value, s = el.selectionStart, e = el.selectionEnd;
      if (s > e) { var tmp = s; s = e; e = tmp; }
      var nv, caret;
      if (inputType === 'insertText') { nv = v.slice(0, s) + data + v.slice(e); caret = s + data.length; }
      else if (inputType === 'deleteContentBackward') { if (s === e && s > 0) s--; nv = v.slice(0, s) + v.slice(e); caret = s; }
      else { if (s === e) e++; nv = v.slice(0, s) + v.slice(e); caret = s; }
      if (el.localName === 'input' && el.type === 'number' && nv !== '' && !/^-?\d*\.?\d*(e-?\d*)?$/i.test(nv)) return true;
      if (el.localName === 'input' && el.hasAttribute('maxlength') && nv.length > el.maxLength && el.maxLength >= 0) return true;
      sw.setValue(el[ID], nv); slots(el).selStart = slots(el).selEnd = caret;
      slots(el).dirtyValue = true;
    }
    fire(el, 'input', { bubbles: true, cancelable: false, inputType: inputType, data: data });
    return true;
  }
  function setValueFromGo(id, value) {
    var el = wrap(+id); if (!el) return false;
    value = value === null || value === undefined ? '' : String(value);
    if (el.localName === 'select') {
      var opts = itemsOf(el.options), idx = -1;
      opts.forEach(function (o, i) { if (idx < 0 && (o.value === value || o.text === value)) idx = i; });
      if (idx >= 0) sw.setSelectedIndex(el[ID], idx); else sw.setValue(el[ID], value);
    } else if (el.localName === 'input' && (el.type === 'checkbox' || el.type === 'radio')) {
      sw.setChecked(el[ID], value === 'true' || value === '1' || value === 'on' || value === 'checked');
    } else if (el.localName === 'input' || el.localName === 'textarea') sw.setValue(el[ID], value);
    else el.textContent = value;
    slots(el).dirtyValue = true;
    fire(el, 'input', { bubbles: true, cancelable: false, inputType: 'insertText', data: value });
    fire(el, 'change', { bubbles: true, cancelable: false });
    return true;
  }
  // the element at a page point (x, y: CSS px from the top-left of the document)
  function elAtPage(x, y) { return host('elementFromPoint') ? wrap(sw.elementFromPoint(+x || 0, +y || 0)) : null; }
  function pointerInit(x, y, el) {
    var p = scrollPos(), cx = (+x || 0) - p[0], cy = (+y || 0) - p[1], ob = el ? hostBox(el[ID]) : null;
    return { clientX: cx, clientY: cy, pageX: +x || 0, pageY: +y || 0, x: cx, y: cy, screenX: cx, screenY: cy,
      offsetX: ob ? (+x || 0) - ob.x : 0, offsetY: ob ? (+y || 0) - ob.y : 0 };
  }
  function mouseFromGo(type, x, y, init) {
    var el = elAtPage(x, y) || documentWrapper.body || documentWrapper;
    type = String(type);
    init = Object.assign(pointerInit(x, y, el), { button: 0, buttons: /down|move/.test(type) && type !== 'mousemove' ? 1 : 0 }, init || {});
    if (type === 'click') return dispatchFromGo(el[ID], 'click', init) ? (defaultAction(el), true) : false;
    var ok = fire(el, type, init);
    if (type === 'mousedown' && ok) focusForPointer(el);
    return ok;
  }
  // the default action of a mousedown: focus the clicked focusable area (the element or its nearest focusable
  // ancestor), or blur the focused element when the press lands on something that takes no focus
  function focusForPointer(el) {
    for (var e = el; e && e.nodeType === 1; e = e.parentNode) {
      var n = e.localName;
      if (((n === 'input' && e.type !== 'hidden') || n === 'textarea' || n === 'select' || n === 'button') && !e.disabled) { focusElement(e); return; }
      if ((n === 'a' && e.hasAttribute('href')) || e.hasAttribute('tabindex') || e.isContentEditable || n === 'summary') { focusElement(e); return; }
    }
    if (activeElement) blurActive(null);
  }
  function touchFromGo(id, type) {
    var el = targetOf(id); if (!el) return true;
    var b = hostBox(el[ID]);
    var touch = { identifier: 1, target: el, clientX: b.x + b.width / 2, clientY: b.y + b.height / 2, pageX: b.x + b.width / 2, pageY: b.y + b.height / 2, screenX: b.x, screenY: b.y, radiusX: 1, radiusY: 1, rotationAngle: 0, force: 1 };
    var list = [touch]; list.item = function (i) { return list[i] || null; };
    var ended = type === 'touchend' || type === 'touchcancel';
    return fire(el, String(type), { touches: ended ? Object.assign([], { item: function () { return null; } }) : list, targetTouches: ended ? [] : list, changedTouches: list });
  }
  function dragFromGo(srcId, dstId) {
    var src = wrap(+srcId), dst = wrap(+dstId);
    if (!src || !dst) return false;
    var dt = new DataTransfer();
    if (!fire(src, 'dragstart', { dataTransfer: dt })) return false;
    fire(src, 'drag', { dataTransfer: dt });
    fire(dst, 'dragenter', { dataTransfer: dt });
    var overPrevented = !fire(dst, 'dragover', { dataTransfer: dt });
    var dropped = false;
    if (overPrevented) { dropped = !fire(dst, 'drop', { dataTransfer: dt }); dt.dropEffect = dt.dropEffect === 'none' ? 'move' : dt.dropEffect; }
    else fire(dst, 'dragleave', { dataTransfer: dt });
    fire(src, 'dragend', { dataTransfer: dt });
    return dropped || overPrevented;
  }
  function wheelFromGo(dx, dy, x, y, mods) {
    var el = (x !== undefined ? elAtPage(x, y) : null) || activeElement || documentWrapper.body || documentWrapper;
    var init = x !== undefined ? pointerInit(x, y, el) : {};
    init.deltaX = +dx || 0; init.deltaY = +dy || 0; init.deltaMode = 0;
    if (mods) { init.ctrlKey = !!(mods & 2); init.shiftKey = !!(mods & 1); init.altKey = !!(mods & 4); init.metaKey = !!(mods & 8); }
    return fire(el, 'wheel', init);
  }
  function fireLifecycle(name) {
    name = String(name);
    switch (name) {
      case 'DOMContentLoaded':
        if (readyState === 'loading') { readyState = 'interactive'; fire(documentWrapper, 'readystatechange', { bubbles: false, cancelable: false }); }
        fire(documentWrapper, 'DOMContentLoaded', { bubbles: true, cancelable: false });
        try { scrollToFragment(); } catch (e) { /* no fragment */ }
        return true;
      case 'load':
        if (readyState !== 'complete') { readyState = 'complete'; fire(documentWrapper, 'readystatechange', { bubbles: false, cancelable: false }); }
        fire(global, 'load', { bubbles: false, cancelable: false });
        fire(global, 'pageshow', { bubbles: false, cancelable: false, persisted: false });
        return true;
      case 'beforeunload': {
        var ev = new global.BeforeUnloadEvent('beforeunload', { cancelable: true });
        dispatch(global, ev);
        return !ev.defaultPrevented && ev.returnValue === true;
      }
      case 'unload':
        fire(global, 'pagehide', { bubbles: false, cancelable: false, persisted: false });
        fire(documentWrapper, 'visibilitychange', { bubbles: true, cancelable: false });
        fire(global, 'unload', { bubbles: false, cancelable: false });
        return true;
      case 'pagehide': fire(global, 'pagehide', { persisted: false }); return true;
      case 'visibilitychange': fire(documentWrapper, 'visibilitychange', { bubbles: true }); return true;
      case 'resize': fire(global, 'resize', {}); return true;
      case 'popstate': fire(global, 'popstate', { state: historyState }); return true;
      case 'hashchange': fire(global, 'hashchange', { oldURL: '', newURL: currentURL() }); return true;
      default: return fire(global, name, {});
    }
  }
  function installDocument() {
    docId = sw.docId();
    wrappers.clear();
    documentWrapper = makeWrapper(docId, Document);
    def(global, 'document', { get: function () { return documentWrapper; }, set: function () { }, enumerable: true });
  }
  function setDocument(url, state) {
    // reset everything per-navigation: wrappers, timers, listeners, observers, focus, history override
    timers.length = 0; runningTimer = null;
    observers.length = 0; pendingObservers.length = 0; moFlushScheduled = false;
    activeElement = null; currentScript = null; urlOverride = null; historyState = null; histEntries.length = 0; histIdx = 0;
    perfMarks = []; perfMeasures = []; timeOrigin = Date.now();
    ceDefinitions.clear(); ceWhenDefined.clear();
    blobURLs.clear();
    var m = global[LISTENERS]; if (m) for (var k in m) delete m[k];
    var h = global[HANDLERS]; if (h) for (var k2 in h) delete h[k2];
    installDocument();
    readyState = state ? String(state) : 'loading';
    void url;
    return true;
  }

  // ---- serialisation for Runtime.evaluate
  function refOf(id) { if (host('refOf')) { try { var r = sw.refOf(id); if (r !== null && r !== undefined) return 'e' + r; } catch (e) { /* ignore */ } } return 'n' + id; }
  function serialize(v, depth, seen) {
    depth = depth || 0;
    if (v === undefined) return undefined;
    if (v === null) return null;
    var t = typeof v;
    if (t === 'number') return isFinite(v) ? v : String(v);
    if (t === 'string' || t === 'boolean') return v;
    if (t === 'bigint') return String(v);
    if (t === 'symbol') return v.toString();
    if (t === 'function') return 'function';
    if (isNodeObj(v)) { var o = { nodeType: v.nodeType, tagName: v.nodeType === 1 ? v.tagName : v.nodeName, id: v.nodeType === 1 ? v.id : '', ref: v[ID] > 0 ? refOf(v[ID]) : null }; if (v.nodeType === 3 || v.nodeType === 8) o.data = v.data; o.sweefRef = o.ref; return o; }
    if (v instanceof Error) return { name: v.name, message: v.message, stack: v.stack || '' };
    if (v instanceof Date) return isNaN(v.getTime()) ? null : v.toISOString();
    if (v instanceof RegExp) return v.toString();
    if (v instanceof URL || v instanceof URLSearchParams) return v.toString();
    if (v instanceof Promise) return '[Promise]';
    if (depth >= 8) return Array.isArray(v) ? '[Array]' : '[Object]';
    seen = seen || [];
    if (seen.indexOf(v) >= 0) return '[Circular]';
    seen.push(v);
    try {
      var out, i;
      if (v instanceof Map) return Array.from(v.entries()).map(function (e) { return [serialize(e[0], depth + 1, seen), serialize(e[1], depth + 1, seen)]; });
      if (v instanceof Set) return Array.from(v).map(function (e) { return serialize(e, depth + 1, seen); });
      if (ArrayBuffer.isView(v)) return Array.prototype.slice.call(v, 0, 1024);
      if (v instanceof ArrayBuffer) return { byteLength: v.byteLength };
      if (Array.isArray(v) || v instanceof NodeList || v instanceof HTMLCollection || v instanceof DOMTokenList) { out = []; var n = Math.min(v.length, 1000); for (i = 0; i < n; i++) out.push(serialize(v[i], depth + 1, seen)); return out; }
      if (v instanceof Event) return { type: v.type, target: serialize(v.target, depth + 1, seen), defaultPrevented: v.defaultPrevented };
      if (v instanceof Headers) { out = {}; v.forEach(function (val, k) { out[k] = val; }); return out; }
      if (v instanceof Response) return { status: v.status, statusText: v.statusText, ok: v.ok, url: v.url, bodyUsed: v.bodyUsed };
      if (v instanceof CSSStyleDeclaration) return v.cssText;
      if (v instanceof Storage) { out = {}; Object.keys(v).forEach(function (k) { out[k] = v.getItem(k); }); return out; }
      if (v === global) return '[Window]';
      if (typeof v.toJSON === 'function') { var j = v.toJSON(); if (j !== v) return serialize(j, depth + 1, seen); }
      out = {};
      var keys = Object.keys(v);
      for (i = 0; i < Math.min(keys.length, 1000); i++) { var sv = serialize(v[keys[i]], depth + 1, seen); if (sv !== undefined) out[keys[i]] = sv; }
      return out;
    } finally { seen.pop(); }
  }

  function exportFn(name, fn) { Object.defineProperty(global, name, { value: fn, writable: true, configurable: true, enumerable: false }); }
  exportFn('__swDispatch', function (id, type, init) { try { return dispatchFromGo(id, type, init); } catch (e) { reportError(e); return true; } });
  exportFn('__swDispatchKey', function (id, key, mods) { try { return dispatchKey(id, key, mods); } catch (e) { reportError(e); return true; } });
  exportFn('__swMouse', function (type, x, y, init) { try { return mouseFromGo(type, x, y, init); } catch (e) { reportError(e); return true; } });
  exportFn('__swTouch', function (id, type) { try { return touchFromGo(id, type); } catch (e) { reportError(e); return true; } });
  exportFn('__swDrag', function (src, dst) { try { return dragFromGo(src, dst); } catch (e) { reportError(e); return false; } });
  exportFn('__swWheel', function (dx, dy, x, y, mods) { try { return wheelFromGo(dx, dy, x, y, mods); } catch (e) { reportError(e); return true; } });
  // true once the page has registered a wheel listener that can cancel (not passive): the host then waits for the page's answer before scrolling
  exportFn('__swWheelNP', function () { return wheelNP; });
  exportFn('__swSetValueFromGo', function (id, value) { try { return setValueFromGo(id, value); } catch (e) { reportError(e); return false; } });
  exportFn('__swRunTimers', function (now, withBg, budget) { try { return runTimers(now, !!withBg, budget); } catch (e) { reportError(e); return timers.length ? 0 : -1; } });
  exportFn('__swFlushMutationObservers', function () { try { flushMutationObservers(); } catch (e) { reportError(e); } return pendingObservers.length; });
  exportFn('__swSetHidden', function (h, quiet) { h = !!h; if (h === pageHidden) return false; pageHidden = h; if (!quiet) { try { fire(documentWrapper, 'visibilitychange', { bubbles: true }); } catch (e) { reportError(e); } } return true; });
  exportFn('__swFireLifecycle', function (name) { try { return fireLifecycle(name); } catch (e) { reportError(e); return true; } });
  exportFn('__swSetDocument', function (url, state) { return setDocument(url, state); });
  exportFn('__swBlockScripts', function () { scriptsBlocked = true; });
  exportFn('__swDefaultAction', function (id) { try { defaultAction(wrap(+id)); return true; } catch (e) { reportError(e); return false; } });
  exportFn('__swSerialize', function (v) { try { return serialize(v); } catch (e) { return { name: e && e.name, message: String(e && e.message || e) }; } });
  exportFn('__swSetCurrentScript', function (id) { currentScript = id ? wrap(+id) : null; return true; });
  exportFn('__swIsThenable', function (v) { return !!v && (typeof v === 'object' || typeof v === 'function') && typeof v.then === 'function'; });
  exportFn('__swTimerCount', function () { return timers.length; });
  exportFn('__swWrap', function (id) { return wrap(+id); });
  exportFn('__swBlobText', function (u) { var b = blobURLs.get(String(u)); return b ? utf8Decode(b._bytes || new Uint8Array(0)) : null; });
  exportFn('__swIdOf', function (node) { return idOf(node); });
  exportFn('__swElScrolled', function (id) { try { var el = wrap(+id); if (el) queueScrollEvent(el); } catch (e) { reportError(e); } return true; });
  exportFn('__swElAtPage', function (x, y) { try { return elAtPage(x, y); } catch (e) { return null; } });

  // A pooled VM is built before it is bound to a document; __swSetDocument
  // installs the real one at bind time.
  try { installDocument(); } catch (e) { /* unbound VM */ }
  readyState = 'loading';
})(globalThis);
