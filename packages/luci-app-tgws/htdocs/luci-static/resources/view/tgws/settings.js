'use strict';
'require view';
'require form';

// Октет IPv4 по смыслу (0–255), а не по числу цифр: значение вроде
// 7:300.1.1.1 форма иначе принимала бы, а демон отказывался бы запускаться.
var OCTET = '(25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})';
var IPV4 = '(' + OCTET + '\\.){3}' + OCTET;

return view.extend({
	render: function() {
		var m, s, o;

		m = new form.Map('tgws', _('Telegram'),
			_('Carries Telegram traffic of every device on the network over WebSocket, which often works where the direct connection is slow or blocked. Nothing needs to be set up on the devices.'));
		// Вкладки внутри одной секции: каждая — отдельная тема настроек, так
		// форма не превращается в длинный список, где нужная опция теряется.
		s = m.section(form.NamedSection, 'main', 'tgws');
		s.addremove = false;
		s.tab('general', _('General'));
		s.tab('websocket', _('WebSocket'));
		s.tab('worker', _('Cloudflare Worker'));
		s.tab('other', _('Other traffic'));
		s.tab('subnets', _('Subnets'));

		// --- Общее -----------------------------------------------------------
		o = s.taboption('general', form.Flag, 'enabled', _('Enable'),
			_('Whether the service starts when the router boots. The Start button on the Status page runs it right now.'));
		o.rmempty = false;

		o = s.taboption('general', form.Value, 'port', _('Local port'),
			_('The port the daemon listens on. Telegram connections are redirected to it by the firewall; change it only if the port is taken.'));
		o.datatype = 'port';
		o.default = '5454';

		o = s.taboption('general', form.Value, 'lan_devices', _('LAN interfaces'),
			_('Space-separated list whose traffic is intercepted. Empty means the device of the lan network. Only interfaces of firewall zones that accept input to the router work: Telegram cannot connect from a zone set to reject.'));
		o.placeholder = 'br-lan';
		o.optional = true;
		o.validate = function(section_id, value) {
			if (value === '' || /^[A-Za-z0-9._ -]+$/.test(value))
				return true;
			return _('Only letters, digits, dots, dashes and spaces are allowed');
		};

		// --- WebSocket ------------------------------------------------------
		o = s.taboption('websocket', form.Value, 'dc_ip', _('WebSocket targets'),
			_('DC:IP pairs, comma-separated. Data centers not listed here are passed through directly. If photos and videos do not load, try leaving only 4:149.154.167.220, or enter none to use no WebSocket at all.'));
		o.default = '2:149.154.167.220,4:149.154.167.220';
		o.value('2:149.154.167.220,4:149.154.167.220', _('DC2 and DC4 (default)'));
		o.value('4:149.154.167.220', _('DC4 only'));
		o.value('none', _('none: no WebSocket, everything goes the direct way'));
		o.validate = function(section_id, value) {
			var pair = '(1|2|3|4|5|203):' + IPV4;
			if (value === '' || value === 'none' ||
			    new RegExp('^' + pair + '(,[ ]*' + pair + ')*$').test(value))
				return true;
			return _('Expected pairs like 2:149.154.167.220, separated by commas, or none');
		};

		o = s.taboption('websocket', form.Value, 'ip_dc', _('Manual address to data center map'),
			_('IP:DC pairs, comma-separated, for Telegram addresses the package could not place by itself. Android does not name the data center in its connection, so for a new address it is worked out automatically; add a pair here only if an address keeps landing among the unknown ones on the Status page.'));
		o.placeholder = '149.154.167.35:2';
		o.optional = true;
		o.validate = function(section_id, value) {
			var pair = IPV4 + ':(1|2|3|4|5|203)';
			if (value === '' || new RegExp('^' + pair + '(,[ ]*' + pair + ')*$').test(value))
				return true;
			return _('Expected pairs like 149.154.167.35:2, separated by commas');
		};

		o = s.taboption('worker', form.Value, 'worker_domain', _('Worker domain'),
			_('When set, Telegram TCP connections try this Worker first. If opening the Worker fails, the existing Telegram WebSocket route is used. Clear the domain to disable.'));
		o.placeholder = 'tgws-relay.example.workers.dev';
		o.optional = true;
		o.validate = function(section_id, value) {
			if (value === '' || (value.length <= 253 &&
			    /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/i.test(value) &&
			    !/^[0-9.]+$/.test(value))) return true;
			return _('Enter a hostname without https://, path or port');
		};
		o = s.taboption('worker', form.Value, 'worker_token', _('Worker access token'),
			_('Use the same secret as TGWS_TOKEN in your Worker. Leave empty only for a Worker that does not require authentication. This is not a Cloudflare API token.'));
		o.password = true;
		o.optional = true;
		o.validate = function(section_id, value) {
			return value === '' || /^[A-Za-z0-9_-]{32,256}$/.test(value)
				? true : _('Use 32-256 letters, digits, underscores or dashes');
		};

		// --- Остальной трафик ------------------------------------------------
		o = s.taboption('other', form.Value, 'fallback_mark', _('Mark for traffic that cannot use WebSocket'),
			_('Telegram traffic that cannot go over WebSocket (websites, data centers without a WebSocket endpoint) is sent directly by default. To send it through the TrustTunnel tunnel instead, enter the tunnel mark, 0x9527 by default. Empty means direct.'));
		o.value('', _('directly (no tunnel)'));
		o.value('0x9527', _('through the TrustTunnel tunnel (0x9527)'));
		o.placeholder = '0x9527';
		o.optional = true;
		o.validate = function(section_id, value) {
			if (value === '' || /^(0[xX][0-9a-fA-F]{1,8}|[0-9]{1,10})$/.test(value))
				return true;
			return _('Expected a number like 0x9527');
		};

		// --- Подсети ---------------------------------------------------------
		o = s.taboption('subnets', form.Value, 'subnets_url', _('Telegram subnet list URL'),
			_('Once a week the router refreshes the list of Telegram subnets from this address. Empty means the official core.telegram.org list. If that address is blocked from the router, the built-in or the last downloaded list stays in use; you can point this to a reachable mirror.'));
		o.placeholder = 'https://core.telegram.org/resources/cidr.txt';
		o.optional = true;
		o.validate = function(section_id, value) {
			if (value === '' || /^https?:\/\/[A-Za-z0-9._~:\/?&=%+-]+$/.test(value))
				return true;
			return _('Expected an http(s) address');
		};

		return m.render();
	}
});
