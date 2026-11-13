import {useEffect,useRef,useState} from 'react'
import {Alert,AppState,Linking,Text,View} from 'react-native'
import * as Crypto from 'expo-crypto'
import {MonarrClient} from '../api'
import {normalizeServerUrl} from '../connection'
import {useResource} from '../useResource'
import {useTheme} from '../theme'
import {Button,InlineError} from '../components/UI'
import type {Connection} from '../types'

export function CinemaSignIn({server,onConnected}:{server:string;onConnected:(connection:Connection)=>Promise<void>}){
 const serverRef=useRef(server);serverRef.current=server
 const theme=useTheme(),[error,setError]=useState(''),[busy,setBusy]=useState(false),[pending,setPending]=useState(false),handoff=useRef<{client:MonarrClient;state:string;verifier:string;expires:number}|null>(null),lock=useRef(false),alive=useRef(true)
 const info=useResource(async()=>{if(!server.trim())return null;return new MonarrClient({baseUrl:normalizeServerUrl(server),apiKey:''}).getSignInInfo()},[server])
 useEffect(()=>{handoff.current=null;setPending(false);setError('')},[server])
 useEffect(()=>{alive.current=true;return()=>{alive.current=false;handoff.current=null}},[])
 const finish=async()=>{
  const h=handoff.current;if(!h||lock.current)return;lock.current=true;setBusy(true)
  try{if(Date.now()>h.expires)throw Error('Cinema sign-in expired. Start again or use local sign-in.');const result=await h.client.finishCinemaNative(h.state,h.verifier);if(!alive.current||handoff.current!==h){if(result)void new MonarrClient({...h.client.connection,sessionToken:result.token,apiKey:''}).logoutAccount().catch(()=>{});return};if(!result){setError('Waiting for Cinema consent. Finish in your browser, then tap Finish sign-in.');return}
   handoff.current=null;setPending(false);setError('');Alert.alert('Use this Curator account?',`${result.account.displayName} · ${result.account.username}. Cinema login permits household requests; owner administration requires local sign-in.`,[{text:'Cancel',style:'cancel',onPress:()=>{void new MonarrClient({...h.client.connection,sessionToken:result.token,apiKey:''}).logoutAccount().catch(()=>{if(alive.current)Alert.alert('Sign-out not confirmed','The unused sign-in could not be revoked. It will expire; local access is unchanged.')})}},{text:'Sign in',onPress:()=>{if(!alive.current||normalizeServerUrl(serverRef.current)!==h.client.connection.baseUrl){void new MonarrClient({...h.client.connection,sessionToken:result.token,apiKey:''}).logoutAccount().catch(()=>{});return};void onConnected({baseUrl:h.client.connection.baseUrl,apiKey:'',sessionToken:result.token}).catch(cause=>setError(cause instanceof Error?cause.message:'Connection could not be saved.'))}}])
  }catch(cause){if(alive.current){setError(cause instanceof Error?cause.message:'Cinema sign-in failed.');handoff.current=null;setPending(false)}}finally{lock.current=false;if(alive.current)setBusy(false)}
 }
 useEffect(()=>{const subscription=AppState.addEventListener('change',state=>{if(state==='active')void finish()});return()=>subscription.remove()},[])
 const start=async()=>{if(lock.current)return;lock.current=true;setBusy(true);setError('');try{const client=new MonarrClient({baseUrl:normalizeServerUrl(server),apiKey:''}),bytes=await Crypto.getRandomBytesAsync(32),verifier=Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join(''),digest=await Crypto.digestStringAsync(Crypto.CryptoDigestAlgorithm.SHA256,verifier,{encoding:Crypto.CryptoEncoding.BASE64}),challenge=digest.replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');const result=await client.startCinemaNative(challenge);if(!alive.current)return;handoff.current={client,state:result.state,verifier,expires:Date.now()+5*60*1000};setPending(true);await Linking.openURL(result.url)}catch(cause){if(alive.current){setError(cause instanceof Error?cause.message:'Cinema sign-in could not start.');handoff.current=null;setPending(false)}}finally{lock.current=false;if(alive.current)setBusy(false)}}
 if(!info.data?.cinema)return null
 return <View style={{gap:10}}><Text style={{color:theme.muted}}>Optional: sign in through an explicitly linked Cinema account. Your Cinema credentials stay in your browser.</Text><Button secondary label={pending?'Finish Cinema sign-in':'Continue with Cinema'} disabled={busy} onPress={()=>void(pending?finish():start())}/>{pending?<Button secondary label="Cancel Cinema sign-in" onPress={()=>{handoff.current=null;setPending(false);setError('')}}/>:null}<InlineError message={error}/></View>
}
