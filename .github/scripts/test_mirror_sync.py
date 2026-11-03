import importlib.util
from pathlib import Path
import tempfile
import unittest
import urllib.request

spec=importlib.util.spec_from_file_location('mirror',str(Path(__file__).with_name('mirror-sync.py')))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
SOURCE='cuijianzhuang/Polaris';TARGET='PolarisPanelOrg/Polaris'
def release(tag='v1',date='2020-01-01T00:00:00Z',pre=False,assets=None):
 return dict(id=10,tag_name=tag,name=None,body='Notes',draft=False,prerelease=pre,created_at=date,published_at=date,assets=assets or [])

class FakeAPI:
 def __init__(self,assets=None,bad_download=False):
  self.calls=[];self.assets=assets or [];self.bad_download=bad_download
 def request(self,path,method='GET',data=None,**kw):
  self.calls.append((path,method,data))
  if '/git/ref/' in path:return {'object':{'sha':'same'}}
  if method=='POST' and path.endswith('/releases'):return dict(id=20,upload_url='https://uploads.github.com/repos/PolarisPanelOrg/Polaris/releases/20/assets{?name,label}')
  if method=='PATCH' and '/releases/' in path:return dict(id=20,upload_url='https://uploads.github.com/repos/PolarisPanelOrg/Polaris/releases/20/assets{?name,label}')
  return None
 def pages(self,path):return self.assets
 def download(self,*args):
  Path(args[-1]).write_bytes(b'bad');return (3,'sha256:bad')

class MirrorTests(unittest.TestCase):
 def test_pair_allowlist(self):
  for repo in m.NAMES:m.validate_pair('cuijianzhuang/'+repo,'PolarisPanelOrg/'+repo)
  for a,b in [(SOURCE,'attacker/Polaris'),('PolarisPanelOrg/Polaris',SOURCE),('cuijianzhuang/other','PolarisPanelOrg/other')]:
   with self.assertRaises(m.SyncError):m.validate_pair(a,b)
 def test_latest_initial_future_versions_and_existing_history(self):
  old=release('old');stable=release('stable','2022-01-01T00:00:00Z');pre=release('pre','2023-01-01T00:00:00Z',True)
  future=release('future-old-version','2025-01-01T00:00:00Z');old_edit=release('old-edit',assets=[{'updated_at':'2025-01-01T00:00:00Z'}])
  items=[old,stable,pre,future,old_edit]
  selected=m.select_releases(items,[],SOURCE,'2024-01-01T00:00:00Z','stable')
  self.assertEqual({r['tag_name'] for r in selected},{'stable','pre','future-old-version','old-edit'})
  target=dict(old,body=m.mirror_body(old,SOURCE))
  selected=m.select_releases(items,[target],SOURCE,'2024-01-01T00:00:00Z','stable')
  self.assertIn('old',{r['tag_name'] for r in selected})
 def test_manual_event_can_import_older_release(self):
  items=[release('old'),release('stable')]
  self.assertEqual(len(m.select_releases(items,[],SOURCE,'2024-01-01T00:00:00Z','stable','old')),2)
 def test_marker_preserves_text_and_nested_previous_comment(self):
  r=release();r['body']='Title\n<!-- polaris-mirror:{"source":"earlier"} -->'
  body=m.mirror_body(r,SOURCE)
  self.assertTrue(body.startswith(r['body']));self.assertEqual(m.marker(body)['source'],SOURCE)
 def test_same_size_changed_source_is_not_treated_as_synced(self):
  asset={'id':1,'size':5,'name':'a','updated_at':'new','digest':None}
  old={'id':1,'size':5,'updated_at':'old','digest':None}
  self.assertFalse(m.same_asset(asset,{'size':5,'digest':None},old))
 def test_checksum_verification_prevents_publication_and_false_provenance(self):
  asset={'id':1,'size':6,'name':'a','updated_at':'new','digest':'sha256:good'}
  source=FakeAPI();target=FakeAPI();r=release(assets=[asset])
  with tempfile.TemporaryDirectory() as tmp:
   with self.assertRaises(m.SyncError):m.sync_release(r,None,SOURCE,TARGET,source,target,'v1',tmp)
  staged=next(data for path,method,data in target.calls if method=='POST')
  self.assertTrue(staged['draft']);self.assertEqual(m.marker(staged['body'])['assets'],{})
  self.assertFalse(any(method=='PATCH' and not data.get('draft',True) for _,method,data in target.calls))
 def test_idempotent_complete_release_does_not_write(self):
  r=release();existing=dict(r,id=20,name='v1',body=m.mirror_body(r,SOURCE))
  source,target=FakeAPI(),FakeAPI()
  with tempfile.TemporaryDirectory() as tmp:self.assertFalse(m.sync_release(r,existing,SOURCE,TARGET,source,target,'v1',tmp))
  self.assertFalse(any(method!='GET' for _,method,_ in target.calls))
 def test_unmanaged_release_is_never_overwritten(self):
  with tempfile.TemporaryDirectory() as tmp:
   with self.assertRaises(m.SyncError):m.sync_release(release(),{'body':'manual'},SOURCE,TARGET,FakeAPI(),FakeAPI(),'v1',tmp)
 def test_visibility_mismatch_blocks_copy_if_update_not_confirmed(self):
  api=FakeAPI()
  with self.assertRaises(m.SyncError):
   m.sync_visibility({'private':True},{'private':False,'full_name':TARGET},api)
  self.assertEqual(api.calls[0][2],{'private':True})
 def test_cross_host_redirect_strips_auth(self):
  req=urllib.request.Request('https://api.github.com/repos/x/releases/assets/1',headers={'Authorization':'Bearer fixture-secret'})
  moved=m.SafeRedirect().redirect_request(req,None,302,'',{},'https://release-assets.githubusercontent.com/path')
  self.assertIsNone(moved.get_header('Authorization'))
  with self.assertRaises(m.SyncError):m.SafeRedirect().redirect_request(req,None,302,'',{},'https://attacker.example/path')

if __name__=='__main__':unittest.main()
