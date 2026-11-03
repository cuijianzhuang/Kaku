#!/usr/bin/env python3
"""One-way GitHub mirror. Only configured personal -> organization pairs are allowed."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

NAMES = {'Kaku'}
MARKER = re.compile(r'\n?<!-- polaris-mirror:(\{[^\n]*\}) -->\s*$')


class SyncError(Exception):
    pass


class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parts = urllib.parse.urlparse(newurl)
        if parts.scheme != 'https' or parts.hostname not in {'api.github.com', 'uploads.github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com'}:
            raise SyncError('GitHub returned an unexpected redirect host')
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if parts.hostname != urllib.parse.urlparse(req.full_url).hostname:
            new.remove_header('Authorization')
        return new


class API:
    def __init__(self, token):
        self.token = token
        self.opener = urllib.request.build_opener(SafeRedirect())

    def request(self, path, method='GET', data=None, missing=False, raw=None, mime=None, length=None):
        url = path if path.startswith('https://') else 'https://api.github.com/' + path.lstrip('/')
        if urllib.parse.urlparse(url).hostname not in {'api.github.com', 'uploads.github.com'}:
            raise SyncError('API request is outside GitHub')
        payload = json.dumps(data).encode() if data is not None else raw
        headers = {'Authorization': 'Bearer ' + self.token, 'Accept': 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'Polaris-Organization-Mirror'}
        if payload is not None:
            headers['Content-Type'] = mime or 'application/json'
        if length is not None:
            headers['Content-Length'] = str(length)
        for attempt in range(5):
            if raw is not None:
                raw.seek(0)
            try:
                req = urllib.request.Request(url, data=payload, method=method, headers=headers)
                with self.opener.open(req, timeout=120) as response:
                    content = response.read()
                    return json.loads(content) if content else None
            except urllib.error.HTTPError as exc:
                if missing and exc.code == 404:
                    return None
                if exc.code in {429, 500, 502, 503, 504} or (exc.code == 403 and exc.headers.get('Retry-After')):
                    wait = min(int(exc.headers.get('Retry-After', 2 ** (attempt + 1))), 60)
                    time.sleep(wait)
                    continue
                # Never echo response bodies, headers or authenticated request objects.
                raise SyncError(f'GitHub API {method} failed with HTTP {exc.code}') from None
            except (TimeoutError, urllib.error.URLError):
                if method != 'GET' or attempt == 4:
                    raise SyncError(f'GitHub API {method} connection failed') from None
                time.sleep(2 ** attempt)
        raise SyncError('GitHub API retries exhausted')

    def pages(self, path):
        result = []
        for page in range(1, 1000):
            sep = '&' if '?' in path else '?'
            items = self.request(f'{path}{sep}per_page=100&page={page}')
            result.extend(items)
            if len(items) < 100:
                return result
        raise SyncError('Unexpected pagination limit')

    def download(self, asset_id, repository, output):
        url = f'https://api.github.com/repos/{repository}/releases/assets/{asset_id}'
        for attempt in range(4):
            try:
                req = urllib.request.Request(url, headers={'Authorization': 'Bearer ' + self.token, 'Accept': 'application/octet-stream', 'User-Agent': 'Polaris-Organization-Mirror', 'X-GitHub-Api-Version': '2022-11-28'})
                digest = hashlib.sha256()
                count = 0
                with self.opener.open(req, timeout=120) as response, open(output, 'wb') as file:
                    while chunk := response.read(1024 * 1024):
                        file.write(chunk)
                        digest.update(chunk)
                        count += len(chunk)
                return count, 'sha256:' + digest.hexdigest()
            except (TimeoutError, urllib.error.URLError, OSError):
                if attempt == 3:
                    raise SyncError('Release asset download failed') from None
                time.sleep(2 ** attempt)


def marker(body):
    match = MARKER.search(body or '')
    if not match:
        return None
    try:
        return json.loads(match.group(1))
    except (ValueError, TypeError):
        return None


def mirror_body(release, source):
    state = {'source': source, 'release': release['id'], 'assets': {a['name']: {'id': a['id'], 'size': a['size'], 'updated_at': a.get('updated_at'), 'digest': a.get('digest')} for a in release['assets']}}
    value = json.dumps(state, ensure_ascii=True, separators=(',', ':')).replace('<', '\\u003c').replace('>', '\\u003e')
    return (release.get('body') or '') + '\n<!-- polaris-mirror:' + value + ' -->'


def select_releases(source_releases, target_releases, source, cutoff, stable_tag, event_tag='', history='latest'):
    if history == 'all':
        return source_releases
    known = {r['tag_name'] for r in target_releases if (marker(r.get('body')) or {}).get('source') == source}
    selected = set(known)
    if stable_tag:
        selected.add(stable_tag)
    pre = [r for r in source_releases if r.get('prerelease') and not r.get('draft')]
    if pre:
        selected.add(max(pre, key=lambda r: r.get('published_at') or r.get('created_at', ''))['tag_name'])
    if event_tag:
        selected.add(event_tag)
    for release in source_releases:
        dates = [release.get('published_at') or release.get('created_at', '')]
        dates.extend(a.get('updated_at', '') for a in release['assets'])
        if any(date >= cutoff for date in dates if date):
            selected.add(release['tag_name'])
    return [r for r in source_releases if r['tag_name'] in selected]


def same_asset(source, target, previous):
    if source['size'] != target['size']:
        return False
    if source.get('digest') and target.get('digest'):
        return source['digest'] == target['digest']
    return previous == {'id': source['id'], 'size': source['size'], 'updated_at': source.get('updated_at'), 'digest': source.get('digest')}


def validate_pair(source, target):
    if source.split('/')[:1] != ['cuijianzhuang'] or source.split('/')[-1] not in NAMES or source.count('/') != 1:
        raise SyncError('Source repository is not an approved mirror source')
    if target != 'PolarisPanelOrg/' + source.split('/')[1]:
        raise SyncError('Destination must be the corresponding PolarisPanelOrg repository')


def git_sync(source, target, source_token, target_token):
    with tempfile.TemporaryDirectory(prefix='polaris-git-mirror-') as tmp:
        helper = Path(tmp, 'credential.py')
        helper.write_text('''import os,sys
fields=dict(line.rstrip("\\n").split("=",1) for line in sys.stdin if "=" in line)
if fields.get("host")=="github.com":
 path=fields.get("path","").removesuffix(".git")
 token=os.environ.get("SOURCE_TOKEN") if path==os.environ["SOURCE_REPOSITORY"] else os.environ.get("TARGET_TOKEN") if path==os.environ["TARGET_REPOSITORY"] else None
 if token: print("username=x-access-token\\npassword="+token)
''')
        env = os.environ.copy()
        for key in list(env):
            if key.startswith('GIT_TRACE') or key in {'GIT_CURL_VERBOSE', 'GIT_CONFIG_PARAMETERS'}:
                env.pop(key, None)
        env.update(SOURCE_TOKEN=source_token, TARGET_TOKEN=target_token, SOURCE_REPOSITORY=source, TARGET_REPOSITORY=target,
                   GIT_TERMINAL_PROMPT='0', GIT_CONFIG_COUNT='3', GIT_CONFIG_KEY_0='credential.helper', GIT_CONFIG_VALUE_0='',
                   GIT_CONFIG_KEY_1='credential.helper', GIT_CONFIG_VALUE_1='!python3 ' + str(helper),
                   GIT_CONFIG_KEY_2='credential.useHttpPath', GIT_CONFIG_VALUE_2='true')
        bare = Path(tmp, 'repo.git')
        commands = [
            ['git', 'init', '--bare', str(bare)],
            ['git', '--git-dir', str(bare), 'fetch', '--no-tags', 'https://github.com/' + source + '.git', '+refs/heads/*:refs/heads/*', '+refs/tags/*:refs/tags/*'],
            # Never force or prune: divergent destination history requires human review.
            ['git', '--git-dir', str(bare), 'push', '--atomic', 'https://github.com/' + target + '.git', 'refs/heads/*:refs/heads/*', 'refs/tags/*:refs/tags/*'],
        ]
        for command in commands:
            result = subprocess.run(command, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=900)
            if result.returncode:
                raise SyncError('Git mirror failed; check permissions or divergent branches/tags (no force-push was attempted)')


def sync_visibility(src, dst, target_api):
    if src['private'] != dst['private']:
        dst = target_api.request('repos/' + dst['full_name'], 'PATCH', {'private': src['private']})
    if not isinstance(dst, dict) or src['private'] != dst.get('private'):
        raise SyncError('Visibility mismatch; refusing to copy any code or assets')
    return dst


def sync_release(release, existing, source, target, source_api, target_api, stable_tag, directory):
    tag = release['tag_name']
    if existing and (marker(existing.get('body')) or {}).get('source') != source:
        raise SyncError('Destination has an unmanaged release; refusing to overwrite it')
    source_ref = source_api.request('repos/' + source + '/git/ref/tags/' + urllib.parse.quote(tag, safe=''), missing=True)
    target_ref = target_api.request('repos/' + target + '/git/ref/tags/' + urllib.parse.quote(tag, safe=''), missing=True)
    if not source_ref or not target_ref or source_ref['object']['sha'] != target_ref['object']['sha']:
        raise SyncError('Release tag is missing or differs; synchronize Git refs first')
    expected_body = mirror_body(release, source)
    assets = target_api.pages('repos/' + target + '/releases/' + str(existing['id']) + '/assets') if existing else []
    previous = (marker(existing.get('body')) or {}).get('assets', {}) if existing else {}
    by_name = {a['name']: a for a in assets}
    changed = [a for a in release['assets'] if not same_asset(a, by_name.get(a['name'], {'size': -1}), previous.get(a['name']))]
    extra = [a for a in assets if a['name'] not in {s['name'] for s in release['assets']}]
    labels = [a for a in release['assets'] if a['name'] in by_name and (a.get('label') or '') != (by_name[a['name']].get('label') or '')]
    if existing and not changed and not extra and not labels and existing.get('name') == (release.get('name') or tag) and existing.get('body') == expected_body and existing['draft'] == release['draft'] and existing['prerelease'] == release['prerelease']:
        return False
    # Provenance records only verified content. Never mark untransferred assets as current.
    staged_body = existing.get('body') if existing else mirror_body(dict(release, assets=[]), source)
    staged = {'name': release.get('name') or tag, 'body': staged_body, 'draft': True, 'prerelease': release['prerelease']}
    if existing:
        current = target_api.request('repos/' + target + '/releases/' + str(existing['id']), 'PATCH', staged)
    else:
        current = target_api.request('repos/' + target + '/releases', 'POST', dict(staged, tag_name=tag))
    for asset in labels:
        target_api.request('repos/' + target + '/releases/assets/' + str(by_name[asset['name']]['id']), 'PATCH', {'label': asset.get('label') or ''})
    for old in extra:
        target_api.request('repos/' + target + '/releases/assets/' + str(old['id']), 'DELETE')
    for asset in changed:
        path = Path(directory, str(asset['id']) + '.asset')
        count, digest = source_api.download(asset['id'], source, path)
        try:
            if count != asset['size'] or (asset.get('digest') and digest != asset['digest']):
                raise SyncError('Source asset failed size or SHA-256 verification')
            if asset['name'] in by_name:
                target_api.request('repos/' + target + '/releases/assets/' + str(by_name[asset['name']]['id']), 'DELETE')
            url = current['upload_url'].split('{', 1)[0] + '?' + urllib.parse.urlencode({'name': asset['name'], 'label': asset.get('label') or ''})
            mime = asset.get('content_type') or 'application/octet-stream'
            if not re.fullmatch(r'[\w.+-]+/[\w.+-]+', mime):
                mime = 'application/octet-stream'
            with path.open('rb') as file:
                uploaded = target_api.request(url, 'POST', raw=file, mime=mime, length=count)
            if uploaded.get('digest'):
                if uploaded['digest'] != digest:
                    raise SyncError('Uploaded asset SHA-256 verification failed')
            else:
                _, check = target_api.download(uploaded['id'], target, path)
                if check != digest:
                    raise SyncError('Uploaded asset verification failed')
        finally:
            path.unlink(missing_ok=True)
    # Re-read before publishing so missing/partial attachments are never published.
    final_assets = target_api.pages('repos/' + target + '/releases/' + str(current['id']) + '/assets')
    final = {a['name']: a for a in final_assets}
    if len(final) != len(release['assets']) or any(a['name'] not in final or a['size'] != final[a['name']]['size'] for a in release['assets']):
        raise SyncError('Release assets are incomplete; the mirror remains a draft')
    target_api.request('repos/' + target + '/releases/' + str(current['id']), 'PATCH',
                       {'name': release.get('name') or tag, 'body': expected_body, 'draft': release['draft'], 'prerelease': release['prerelease'],
                        'make_latest': 'true' if not release['draft'] and not release['prerelease'] and tag == stable_tag else 'false'})
    return True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', required=True)
    parser.add_argument('--target', required=True)
    parser.add_argument('--mode', choices=['all', 'code', 'releases', 'check'], default='all')
    parser.add_argument('--history', choices=['latest', 'all'], default='latest')
    parser.add_argument('--event-tag', default='')
    args = parser.parse_args()
    validate_pair(args.source, args.target)
    source_token = os.environ.get('SOURCE_TOKEN')
    target_token = os.environ.get('TARGET_TOKEN')
    if not source_token or not target_token:
        raise SyncError('SOURCE_TOKEN and TARGET_TOKEN are required; configure MIRROR_TOKEN in the source repository')
    source_api, target_api = API(source_token), API(target_token)
    src = source_api.request('repos/' + args.source)
    dst = target_api.request('repos/' + args.target)
    if src['full_name'] != args.source or dst['full_name'] != args.target:
        raise SyncError('Repository identity changed; refusing to follow a renamed owner')
    if args.mode == 'check':
        print(json.dumps({'source': args.source, 'target': args.target, 'sourcePrivate': src['private'], 'targetPrivate': dst['private']}))
        return
    dst = sync_visibility(src, dst, target_api)
    target_api.request('repos/' + args.target + '/actions/permissions', 'PUT', {'enabled': False})
    if args.mode in {'all', 'code'}:
        git_sync(args.source, args.target, source_token, target_token)
        if dst['default_branch'] != src['default_branch']:
            target_api.request('repos/' + args.target, 'PATCH', {'default_branch': src['default_branch']})
        print('Code and tags synchronized: ' + args.target, flush=True)
    if args.mode in {'all', 'releases'}:
        releases = source_api.pages('repos/' + args.source + '/releases')
        targets = target_api.pages('repos/' + args.target + '/releases')
        latest = source_api.request('repos/' + args.source + '/releases/latest', missing=True)
        stable = latest['tag_name'] if latest else None
        selected = select_releases(releases, targets, args.source, dst['created_at'], stable, args.event_tag, args.history)
        existing = {r['tag_name']: r for r in targets}
        with tempfile.TemporaryDirectory(prefix='polaris-release-mirror-') as tmp:
            for release in sorted(selected, key=lambda r: r.get('published_at') or r.get('created_at', '')):
                changed = sync_release(release, existing.get(release['tag_name']), args.source, args.target, source_api, target_api, stable, tmp)
                print(json.dumps({'release': release['tag_name'], 'assets': len(release['assets']), 'updated': changed}), flush=True)
        print('Release reconciliation complete: ' + args.target, flush=True)


if __name__ == '__main__':
    os.umask(0o077)
    try:
        main()
    except (SyncError, subprocess.TimeoutExpired) as exc:
        print('Mirror failed: ' + str(exc), flush=True)
        raise SystemExit(1)
