'use strict';
'require view';
'require rpc';
'require poll';
'require dom';
'require ui';

var callStatus = rpc.declare({ object: 'luci.tgws', method: 'status' });
var callService = rpc.declare({ object: 'luci.tgws', method: 'service', params: [ 'action' ] });
var callLog = rpc.declare({ object: 'luci.tgws', method: 'log', params: [ 'lines' ] });
var callVersions = rpc.declare({ object: 'luci.tgws', method: 'versions', params: [ 'refresh' ] });

function row(label, value) {
	return E('tr', { 'class': 'tr' }, [
		E('td', { 'class': 'td left', 'width': '38%' }, label),
		E('td', { 'class': 'td left' }, value)
	]);
}

function fmtBytes(n) {
	if (!n) return '0';
	var u = [ 'B', 'KB', 'MB', 'GB', 'TB' ], i = 0;
	while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
	return (i ? n.toFixed(1) : String(n)) + ' ' + u[i];
}

function fmtAge(ts) {
	if (!ts) return _('never');
	var d = Math.floor(Date.now() / 1000) - ts;
	if (d < 60) return _('just now');
	if (d < 3600) return Math.floor(d / 60) + ' ' + _('min ago');
	if (d < 86400) return Math.floor(d / 3600) + ' ' + _('h ago');
	return Math.floor(d / 86400) + ' ' + _('days ago');
}

function fmtDuration(sec) {
	if (sec < 60) return sec + ' ' + _('s');
	if (sec < 3600) return Math.floor(sec / 60) + ' ' + _('min');
	if (sec < 86400) return Math.floor(sec / 3600) + ' ' + _('h') + ' ' + Math.floor(sec % 3600 / 60) + ' ' + _('min');
	return Math.floor(sec / 86400) + ' ' + _('d') + ' ' + Math.floor(sec % 86400 / 3600) + ' ' + _('h');
}

// Одно предложение о том, что происходит, вместо россыпи флагов: человеку нужен
// ответ «работает ли», а не состояние трёх подсистем. Проверки идут от самой
// базовой причины к следствиям: нет демона — незачем говорить про правила.
function verdict(st) {
	if (!st.installed)
		return { level: 'danger', head: _('The tgws daemon is not installed'),
			detail: _('Run install.sh again: it installs the daemon next to this package.') };
	if (!st.enabled)
		return { level: 'info', head: _('Telegram acceleration is off'),
			detail: _('Turn it on in Settings and press Save & Apply, or press Start to run it right now.') };
	if (!st.running)
		return { level: 'danger', head: _('The service is not running'),
			detail: _('Press Start and read the log below.') };
	if (!st.ready)
		return { level: 'warning', head: _('The service is starting'),
			detail: _('The daemon has not reported in yet. If this persists, the log below says why.') };
	if (!st.intercepting)
		return { level: 'warning', head: _('The daemon runs, but Telegram traffic is not intercepted'),
			detail: _('The firewall rules are not loaded, for example after the firewall was stopped. Telegram works as before. Press Restart, or see the log below.') };
	return { level: 'success', head: _('Telegram is accelerated for every device on the network'), detail: '' };
}

return view.extend({
	load: function() {
		return callStatus();
	},

	handleAction: function(action) {
		ui.showModal(_('Please wait'), [ E('p', { 'class': 'spinning' }, _('Running…')) ]);
		return callService(action).then(function(res) {
			ui.hideModal();
			if (res && res.not_running)
				ui.addNotification(null, E('p', {}, _('The service did not start. The log below says why.')), 'warning');
			else if (res && res.code !== 0)
				ui.addNotification(null, E('pre', {}, res.output || _('Command failed')), 'warning');
			else if (action === 'update_subnets')
				ui.addNotification(null, E('p', {}, (res.output || '').trim() || _('Done')), 'info');
			else
				ui.addNotification(null, E('p', {}, _('Done')), 'info');
		}).catch(function(e) {
			ui.hideModal();
			ui.addNotification(null, E('p', {}, e.message || String(e)), 'danger');
		});
	},

	// Вердикт и факты рисуются ПОРОЗНЬ: вердикт занимает всю ширину, а факты
	// стоят в паре с версиями. Опрос обновляет оба блока из одного вызова.
	//
	// Полоса-вердикт показывается ТОЛЬКО когда что-то не так: тогда в ней
	// подсказка, что делать. Когда всё работает, полоса во всю ширину
	// сообщала бы «всё хорошо» — занимала бы самое видное место, не давая
	// повода к действию. Тогда состояние стоит обычной строкой в таблице.
	renderVerdict: function(st) {
		var v = verdict(st);
		if (v.level === 'success')
			return E('div', {});
		return E('div', { 'class': 'alert-message ' + v.level },
			v.detail
				? [ E('strong', {}, v.head), E('br'), v.detail ]
				: [ E('strong', {}, v.head) ]);
	},

	renderFacts: function(st) {
		var v = verdict(st);
		var s = st.stats || {};
		var sub = st.subnets || {};
		var rows = [ row(_('State'), v.level === 'success'
			? E('strong', {}, _('working'))
			: v.head) ];

		if (st.running && st.ready) {
			rows.push(row(_('Interception'),
				st.intercepting ? _('on, port %s').format(st.port) : _('off')));
			if (s.started)
				rows.push(row(_('Running for'), fmtDuration(Math.max(0, (s.updated || s.started) - s.started))));
		}

		rows.push(row(_('Cloudflare Worker'), st.worker_domain || _('off')));
		if (st.worker_domain)
			rows.push(row(_('Worker routing'), st.worker_mode === 'prefer'
				? _('Prefer Worker for all Telegram TCP connections')
				: _('Fallback: Telegram WebSocket, then Worker, then direct TCP')));
		rows.push(row(_('WebSocket targets'),
			st.dc_ip === 'none' ? _('none: everything goes the direct way') : E('code', {}, st.dc_ip)));
		rows.push(row(_('Other Telegram traffic'),
			st.fallback_mark
				? _('through the TrustTunnel tunnel (mark %s)').format(st.fallback_mark)
				: _('directly')));
		rows.push(row(_('Subnets'),
			sub.custom
				? _('%d, updated %s').format(sub.count, fmtAge(sub.updated))
				: _('%d, built-in list').format(sub.count || 0)));

		if (st.running && st.ready && s.total !== undefined) {
			var share = s.total > 0 ? Math.round(100 * (s.ws || 0) / s.total) : 0;
			rows.push(row(_('Connections'),
				_('%d total, %d active').format(s.total || 0, s.active || 0)));
			rows.push(row(_('Through WebSocket'),
				(s.ws || 0) + (s.total > 0 ? ' (' + share + '%)' : '')));
			rows.push(row(_('Passed through directly'), String(s.fallback || 0)));
			rows.push(row(_('Through Cloudflare Worker'), String(s.worker || 0)));
			rows.push(row(_('Worker connection errors'), String(s.worker_errors || 0)));
			rows.push(row(_('Sent / received'), fmtBytes(s.bytes_up) + ' / ' + fmtBytes(s.bytes_down)));
			var unknown = s.unknown_dc || [];
			if (unknown.length)
				rows.push(row(_('Addresses with no known data center'),
					E('span', { 'style': 'color:#ef6c00' }, unknown.join(', '))));
		}

		return E('table', { 'class': 'table' }, rows);
	},

	// Версии: пакет и демон отдельно. Сетевая часть кэшируется, а кнопка нужна
	// потому, что кэш лежит в /var (tmpfs) и иначе о вышедшем релизе раньше
	// истечения кэша можно узнать только перезагрузкой.
	renderVersions: function(v, box) {
		var self = this;
		function state(latest, upd, ahead) {
			if (latest == null)
				return E('em', {}, _('not checked'));
			if (upd)
				return E('strong', {}, _('%s is available').format(latest));
			if (ahead)
				return _('the installed version is newer than the latest release (%s)').format(latest);
			return _('up to date');
		}
		function verRow(label, installed, missing, st) {
			return row(label, installed
				? E('span', {}, st ? [ installed, ' — ', st ] : installed)
				: E('em', {}, missing));
		}

		var rows = [
			verRow(_('Package'), v.package, _('unknown'),
				state(v.latest, v.update_available, v.ahead)),
			verRow(_('Daemon'), v.daemon, _('not installed'), null)
		];

		var notes = [];
		if (v.update_available)
			notes.push(_('Run install.sh again to update: it refreshes both the package and the daemon.'));
		if (v.stale)
			notes.push(_('GitHub unreachable, showing the last cached result'));
		if (v.package && v.latest == null)
			notes.push(_('Update check unavailable: no network and no cached result'));

		var parts = [ E('table', { 'class': 'table' }, rows) ];
		if (notes.length)
			parts.push(E('div', { 'style': 'margin-top:.5em' }, notes.map(function(t) {
				return E('p', { 'style': 'margin:0' }, t);
			})));

		parts.push(E('div', { 'style': 'margin-top:.5em' }, E('button', {
			'class': 'cbi-button cbi-button-neutral',
			'click': ui.createHandlerFn(this, function() {
				return callVersions(true).then(function(nv) {
					dom.content(box, self.renderVersions(nv, box));
				}).catch(function(e) {
					ui.addNotification(null, E('p', {}, e.message || String(e)), 'danger');
				});
			})
		}, _('Check now'))));

		return E('div', {}, parts);
	},

	render: function(st) {
		var self = this;
		var verdictBox = E('div', {}, this.renderVerdict(st));
		var factsBox = E('div', {}, this.renderFacts(st));
		var versionBox = E('div', {}, E('em', {}, _('Checking…')));
		var logBox = E('pre', { 'style': 'max-height:22em;overflow:auto;margin:0' }, '');

		// Версии запрашиваются ОДИН раз при отрисовке, а не через poll: сетевая
		// часть кэшируется на сутки, и повторять даже кэшированный вызов
		// каждые десять секунд незачем.
		callVersions(false).then(function(v) {
			dom.content(versionBox, self.renderVersions(v, versionBox));
		}).catch(function(e) {
			dom.content(versionBox, E('em', {}, e.message || String(e)));
		});

		poll.add(function() {
			return callStatus().then(function(s) {
				dom.content(verdictBox, self.renderVerdict(s));
				dom.content(factsBox, self.renderFacts(s));
			});
		}, 10);

		poll.add(function() {
			return callLog(80).then(function(r) {
				logBox.textContent = (r.lines || []).join('\n');
			});
		}, 10);

		// Два блока рядом через flex-wrap, а не через сетку с media-запросами:
		// оба узкие и самостоятельные, а flex-basis сам заставляет их встать в
		// столбик на телефоне.
		var pair = E('div', { 'style': 'display:flex;flex-wrap:wrap;gap:0 1.5em' }, [
			E('div', { 'style': 'flex:1 1 24em;min-width:0' }, [
				E('h3', {}, _('Now')),
				factsBox
			]),
			E('div', { 'style': 'flex:1 1 24em;min-width:0' }, [
				E('h3', {}, _('Versions')),
				versionBox
			])
		]);

		return E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, _('Telegram')),

			E('div', { 'class': 'cbi-section' }, [
				verdictBox,
				E('div', { 'style': 'margin-top:1em' }, [
					E('button', { 'class': 'cbi-button cbi-button-apply',
						'click': ui.createHandlerFn(this, 'handleAction', 'start') }, _('Start')),
					' ',
					E('button', { 'class': 'cbi-button cbi-button-reset',
						'click': ui.createHandlerFn(this, 'handleAction', 'stop') }, _('Stop')),
					' ',
					E('button', { 'class': 'cbi-button cbi-button-neutral',
						'click': ui.createHandlerFn(this, 'handleAction', 'restart') }, _('Restart')),
					' ',
					E('button', { 'class': 'cbi-button cbi-button-neutral',
						'click': ui.createHandlerFn(this, 'handleAction', 'update_subnets') }, _('Update subnet list'))
				])
			]),

			E('div', { 'class': 'cbi-section' }, [ pair ]),

			E('div', { 'class': 'cbi-section' }, [
				E('h3', {}, _('Log')),
				logBox
			])
		]);
	},

	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
