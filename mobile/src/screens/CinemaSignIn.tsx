import {useEffect,useRef,useState,type MutableRefObject} from 'react'
import {Alert,Linking,Text,View} from 'react-native'
import * as Crypto from 'expo-crypto'
import {MonarrClient} from '../api'
import {normalizeServerUrl} from '../connection'
import {useResource} from '../useResource'
import {useTheme} from '../theme'
import {Button,InlineError} from '../components/UI'
import type {Connection} from '../types'

type Handoff={client:MonarrClient;state:string;verifier:string;completion?:string;expires:number;epoch:number;server:string}
export function CinemaSignIn({server,onConnected,attempts:shared}:{server:string;onConnected:(connection:Connection)=>Promise<void>;attempts?:MutableRefObject<number>}){
 const own=useRef(0),attempts=shared??own,serverRef=useRef(server)
 if(serverRef.current!==server){serverRef.current=server;if(!shared)attempts.current++}
 const theme=useTheme(),[error,setError]=useState(''),[busy,setBusy]=useState(false),[pending,setPending]=useState(false),handoff=useRef<Handoff|null>(null),lock=useRef(false),alive=useRef(true)
 const info=useResource(async()=>{if(!server.trim())return null;return new MonarrClient({baseUrl:normalizeServerUrl(server),apiKey:''}).getSignInInfo()},[server])
 const current=(h:Handoff)=>alive.current&&attempts.current===h.epoch&&serverRef.current===h.server
 const revoke=async(h:Handoff,token:string)=>{try{await new MonarrClient({...h.client.connection,sessionToken:token,apiKey:''}).logoutAccount()}catch{if(alive.current)Alert.alert('Sign-out not confirmed','The unused sign-in could not be revoked. It will expire; local access is unchanged.')}}
 useEffect(()=>{handoff.current=null;setPending(false);setError('')},[server])
 useEffect(()=>{alive.current=true;return()=>{alive.current=false;attempts.current++;handoff.current=null}},[])
 const finish=async()=>{
  const h=handoff.current;if(!h||lock.current||!current(h))return
  if(!h.completion){setError('Finish Cinema consent in your browser, then return to this device using the Cinema return link.');return}
  lock.current=true;setBusy(true)
  try{
   if(Date.now()>h.expires)throw Error('Cinema sign-in expired. Start again or use local sign-in.')
   const result=await h.client.finishCinemaNative(h.state,h.verifier,h.completion)
   if(!result)throw Error('Cinema consent has not finished.')
   if(!current(h)||handoff.current!==h){await revoke(h,result.token);return}
   handoff.current=null;setPending(false);setError('')
   Alert.alert('Use this Curator account?',`${result.account.displayName} · ${result.account.username}. Cinema login permits household requests; owner administration requires local sign-in.`,[
    {text:'Cancel',style:'cancel',onPress:()=>{void revoke(h,result.token)}},
    {text:'Sign in',onPress:()=>{void(async()=>{if(!current(h)){await revoke(h,result.token);return}try{await onConnected({baseUrl:h.client.connection.baseUrl,apiKey:'',sessionToken:result.token})}catch(cause){await revoke(h,result.token);if(current(h))setError(cause instanceof Error?cause.message:'Connection could not be saved.')}})()}}
   ])
  }catch(cause){if(current(h)){setError(cause instanceof Error?cause.message:'Cinema sign-in failed.');handoff.current=null;setPending(false)}}finally{lock.current=false;if(alive.current)setBusy(false)}
 }
 useEffect(()=>{
  const receive=({url}:{url:string})=>{try{const uri=new URL(url),h=handoff.current;if(uri.protocol!=='monarr:'||uri.hostname!=='cinema-complete'||!h||!current(h)||uri.searchParams.get('state')!==h.state)return;const code=uri.searchParams.get('code');if(!code||code.length<32||code.length>256)return;h.completion=code;void finish()}catch{/* Invalid or unrelated deep link. */}}
  const subscription=Linking.addEventListener('url',receive);return()=>subscription.remove()
 },[])
 const start=async()=>{
  if(lock.current)return
  lock.current=true;setBusy(true);setError('');const epoch=++attempts.current,selected=server
  const valid=()=>alive.current&&attempts.current===epoch&&serverRef.current===selected
  try{
   const client=new MonarrClient({baseUrl:normalizeServerUrl(selected),apiKey:''}),bytes=await Crypto.getRandomBytesAsync(32),verifier=Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join(''),digest=await Crypto.digestStringAsync(Crypto.CryptoDigestAlgorithm.SHA256,verifier,{encoding:Crypto.CryptoEncoding.BASE64}),challenge=digest.replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')
   if(!valid())return
   const result=await client.startCinemaNative(challenge)
   if(!valid())return
   handoff.current={client,state:result.state,verifier,expires:Date.now()+5*60*1000,epoch,server:selected};setPending(true)
   await Linking.openURL(result.url)
  }catch(cause){if(valid()){setError(cause instanceof Error?cause.message:'Cinema sign-in could not start.');handoff.current=null;setPending(false)}}finally{lock.current=false;if(alive.current)setBusy(false);if(handoff.current?.completion)void finish()}
 }
 if(!info.data?.cinema)return null
 return <View style={{gap:10}}><Text style={{color:theme.muted}}>Optional: sign in through an explicitly linked Cinema account. Return from your browser on this same device.</Text><Button secondary label={pending?'Finish Cinema sign-in':'Continue with Cinema'} disabled={busy} onPress={()=>void(pending?finish():start())}/>{pending?<Button secondary label="Cancel Cinema sign-in" onPress={()=>{attempts.current++;handoff.current=null;setPending(false);setError('')}}/>:null}<InlineError message={error}/></View>
}
